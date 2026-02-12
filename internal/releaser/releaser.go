package releaser

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"orchestration-api-go/internal/api/middleware"
	"orchestration-api-go/internal/config"
	"orchestration-api-go/internal/models"
	"orchestration-api-go/internal/redisclient"
)

// Releaser handles releasing pods back to their source pools
type Releaser struct {
	redis  *redisclient.Client
	config *config.Config
	logger *zap.Logger
}

// NewReleaser creates a new Releaser instance
func NewReleaser(redis *redisclient.Client, config *config.Config, logger *zap.Logger) *Releaser {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Releaser{
		redis:  redis,
		config: config,
		logger: logger,
	}
}

// Release releases a pod back to its source pool using the call SID
// It performs the following steps:
// 1. Get call info from Redis
// 2. Check if pod is draining
// 3. Return pod to appropriate pool based on source pool type
// 4. Handle lease (delete if no more connections, keep if shared with active calls)
// 5. Update pod info status
// 6. Delete call info
func (r *Releaser) Release(ctx context.Context, callSID string) (*models.ReleaseResult, error) {
	client := r.redis.GetRedis()

	// 1. Get call info from Redis
	callKey := redisclient.CallInfoKey(callSID)
	callInfo, err := client.HGetAll(ctx, callKey).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get call info: %w", err)
	}

	// 2. Check if call exists
	if len(callInfo) == 0 {
		return nil, ErrCallNotFound
	}

	// 3. Extract pod name and source pool
	podName := callInfo["pod_name"]
	sourcePool := callInfo["source_pool"]

	if podName == "" || sourcePool == "" {
		return nil, fmt.Errorf("incomplete call info: missing pod_name or source_pool")
	}

	// 4. Check if pod is draining
	drainingKey := redisclient.DrainingKey(podName)
	isDraining, err := client.Exists(ctx, drainingKey).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to check draining status: %w", err)
	}

	wasDraining := isDraining > 0
	var releasedToPool string
	var newScore int64 = -1 // -1 indicates not applicable (exclusive pool)

	// 5. Release pod to appropriate pool (if not draining)
	if !wasDraining {
		tier, isMerchant := parseSourcePool(sourcePool)

		if isMerchant {
			// Merchant pool: SADD to merchant's pod set
			poolKey := redisclient.MerchantPodsKey(tier)
			if err := releaseToExclusivePool(ctx, client, poolKey, podName); err != nil {
				return nil, fmt.Errorf("failed to release to merchant pool: %w", err)
			}
			releasedToPool = sourcePool
		} else {
			// Regular pool: check if shared or exclusive
			isShared := r.config.IsSharedTier(tier)
			poolKey := redisclient.PoolAvailableKey(tier)

			if isShared {
				// Shared pool: use Lua script for atomic ZINCRBY -1
				newScore, err = releaseToSharedPool(ctx, client, poolKey, podName)
				if err != nil {
					return nil, fmt.Errorf("failed to release to shared pool: %w", err)
				}
				if newScore >= 0 {
					releasedToPool = sourcePool
				}
			} else {
				// Exclusive pool: SADD to pool
				if err := releaseToExclusivePool(ctx, client, poolKey, podName); err != nil {
					return nil, fmt.Errorf("failed to release to exclusive pool: %w", err)
				}
				releasedToPool = sourcePool
			}
		}
	}

	// 6. Handle lease
	// For shared pools: only delete lease if new score <= 0 (no active calls)
	// For exclusive pools: always delete lease
	leaseKey := redisclient.LeaseKey(podName)
	isShared := false
	if !wasDraining && releasedToPool != "" {
		tier, isMerchant := parseSourcePool(sourcePool)
		if !isMerchant {
			isShared = r.config.IsSharedTier(tier)
		}
	}

	if shouldDeleteLease(isShared, newScore) {
		if err := client.Del(ctx, leaseKey).Err(); err != nil {
			// Log error but don't fail the release
			r.logger.Warn("failed to delete lease", zap.String("pod_name", podName), zap.Error(err))
		}
	}

	// 7. Remove call from pod's active calls SET
	podCallsKey := redisclient.PodCallsKey(podName)
	if err := client.SRem(ctx, podCallsKey, callSID).Err(); err != nil {
		r.logger.Warn("failed to remove call from pod calls set",
			zap.String("pod_name", podName), zap.String("call_sid", callSID), zap.Error(err))
	}

	// 7b. Update pod info status
	podKey := redisclient.PodInfoKey(podName)
	status := "available"
	if wasDraining {
		status = "draining"
	}

	now := time.Now().Unix()
	update := map[string]interface{}{
		"status":             status,
		"allocated_call_sid": "",
		"allocated_at":       "",
		"released_at":        now,
	}

	if err := client.HSet(ctx, podKey, update).Err(); err != nil {
		// Log error but don't fail the release
		r.logger.Warn("failed to update pod info", zap.String("pod_name", podName), zap.Error(err))
	}

	// 8. Delete call info
	if err := client.Del(ctx, callKey).Err(); err != nil {
		// Log error but don't fail the release
		r.logger.Warn("failed to delete call info", zap.String("call_sid", callSID), zap.Error(err))
	}

	middleware.ReleasesTotal.WithLabelValues(sourcePool, "success").Inc()
	middleware.ActiveCalls.Dec()

	return &models.ReleaseResult{
		Success:        true,
		PodName:        podName,
		ReleasedToPool: releasedToPool,
		WasDraining:    wasDraining,
	}, nil
}

// parseSourcePool parses the source pool string and returns the tier/merchant and whether it's a merchant pool
// Source pool format:
// - "merchant:9shines" → ("9shines", true) - dedicated merchant pool
// - "pool:gold" → ("gold", false) - gold exclusive pool
// - "pool:basic" → ("basic", false) - basic shared pool
func parseSourcePool(sourcePool string) (tier string, isMerchant bool) {
	if strings.HasPrefix(sourcePool, "merchant:") {
		return strings.TrimPrefix(sourcePool, "merchant:"), true
	}
	if strings.HasPrefix(sourcePool, "pool:") {
		return strings.TrimPrefix(sourcePool, "pool:"), false
	}
	// Fallback: treat as tier directly
	return sourcePool, false
}

// shouldDeleteLease determines if the lease should be deleted based on pool type and new score
// For shared pools: delete lease only if new score <= 0 (no active calls)
// For exclusive pools: always delete lease (newScore will be -1)
func shouldDeleteLease(isShared bool, newScore int64) bool {
	if !isShared {
		// Exclusive pool: always delete lease
		return true
	}
	// Shared pool: delete lease only if score <= 0
	return newScore <= 0
}
