// Package drainer handles graceful pod draining for rolling updates.
// Draining pods should NOT receive new allocations, but active calls can continue.
package drainer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"orchestration-api-go/internal/config"
	"orchestration-api-go/internal/models"
	"orchestration-api-go/internal/redisclient"
)

// Drainer handles graceful pod draining for rolling updates
type Drainer struct {
	redis  *redisclient.Client
	config *config.Config
	logger *zap.Logger
}

// NewDrainer creates a new Drainer instance
func NewDrainer(redis *redisclient.Client, config *config.Config, logger *zap.Logger) *Drainer {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Drainer{
		redis:  redis,
		config: config,
		logger: logger,
	}
}

// Drain initiates graceful draining of a pod
// It performs the following steps:
// 1. Check if pod has active lease (active call)
// 2. Get pod's tier
// 3. Remove from available pools
// 4. Mark as draining with TTL
// 5. Build appropriate message based on active call status
func (d *Drainer) Drain(ctx context.Context, podName string) (*models.DrainResult, error) {
	client := d.redis.GetRedis()

	// 1. Check if pod has active lease
	leaseKey := redisclient.LeaseKey(podName)
	hasActiveCall, err := client.Exists(ctx, leaseKey).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to check active lease: %w", err)
	}

	// 2. Get pod's tier
	tierKey := redisclient.PodTierKey(podName)
	tier, err := client.Get(ctx, tierKey).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, fmt.Errorf("pod %s not found (no tier assigned)", podName)
		}
		return nil, fmt.Errorf("failed to get pod tier: %w", err)
	}

	// 3. Remove from available pools
	if err := d.removeFromAvailable(ctx, podName, tier); err != nil {
		return nil, fmt.Errorf("failed to remove from available pools: %w", err)
	}

	// 4. Mark as draining with TTL.
	// This MUST succeed — if it fails the pod is removed from available but not
	// marked as draining, making it invisible to the allocator AND zombie cleanup
	// won't recover it (draining check would pass if key doesn't exist, but
	// the pod isn't in the available pool anymore). Rollback by re-adding to
	// the available pool on failure.
	drainingKey := redisclient.DrainingKey(podName)
	drainingTTL := d.config.DrainingTTL
	if drainingTTL == 0 {
		drainingTTL = 6 * time.Minute
	}
	if err := client.Set(ctx, drainingKey, "true", drainingTTL).Err(); err != nil {
		d.logger.Error("failed to set draining key, rolling back pool removal",
			zap.String("pod", podName),
			zap.String("tier", tier),
			zap.Error(err))
		if rbErr := d.reAddToAvailable(ctx, podName, tier); rbErr != nil {
			d.logger.Error("rollback failed — pod invisible until zombie recovery",
				zap.String("pod", podName),
				zap.Error(rbErr))
		}
		return nil, fmt.Errorf("failed to set draining status: %w", err)
	}

	// 5. Build appropriate message
	var message string
	if hasActiveCall > 0 {
		message = fmt.Sprintf("Pod %s is draining with active call in progress. Will complete when call ends.", podName)
	} else {
		message = fmt.Sprintf("Pod %s is draining. No active calls.", podName)
	}

	return &models.DrainResult{
		Success:       true,
		PodName:       podName,
		HasActiveCall: hasActiveCall > 0,
		Message:       message,
	}, nil
}

// removeFromAvailable removes a pod from its available pool based on tier type
// For merchant tiers: removes from merchant's pod set
// For shared tiers: removes from sorted set
// For exclusive tiers: removes from set
func (d *Drainer) removeFromAvailable(ctx context.Context, podName, tier string) error {
	client := d.redis.GetRedis()

	poolKey, isMerchant, isShared := d.parseTier(tier)

	if isMerchant {
		// Merchant tier: remove from merchant's pod set (SREM)
		merchantPodsKey := redisclient.MerchantPodsKey(poolKey)
		if err := client.SRem(ctx, merchantPodsKey, podName).Err(); err != nil {
			d.logger.Error("failed to remove pod from merchant pool",
				zap.String("pod", podName),
				zap.String("key", merchantPodsKey),
				zap.Error(err))
			return fmt.Errorf("failed to remove from merchant pool %s: %w", merchantPodsKey, err)
		}
	} else if isShared {
		// Shared tier: remove from sorted set (ZREM)
		availableKey := redisclient.PoolAvailableKey(poolKey)
		if err := client.ZRem(ctx, availableKey, podName).Err(); err != nil {
			d.logger.Error("failed to remove pod from shared pool",
				zap.String("pod", podName),
				zap.String("key", availableKey),
				zap.Error(err))
			return fmt.Errorf("failed to remove from shared pool %s: %w", availableKey, err)
		}
	} else {
		// Exclusive tier: remove from set (SREM)
		availableKey := redisclient.PoolAvailableKey(poolKey)
		if err := client.SRem(ctx, availableKey, podName).Err(); err != nil {
			d.logger.Error("failed to remove pod from exclusive pool",
				zap.String("pod", podName),
				zap.String("key", availableKey),
				zap.Error(err))
			return fmt.Errorf("failed to remove from exclusive pool %s: %w", availableKey, err)
		}
	}

	return nil
}

// reAddToAvailable is the rollback path for a failed drain — it re-adds the pod
// to the available pool it was just removed from. Best-effort: if this also
// fails, zombie cleanup will recover the pod within 30s.
func (d *Drainer) reAddToAvailable(ctx context.Context, podName, tier string) error {
	client := d.redis.GetRedis()
	poolKey, isMerchant, isShared := d.parseTier(tier)

	if isMerchant {
		return client.SAdd(ctx, redisclient.MerchantPodsKey(poolKey), podName).Err()
	} else if isShared {
		// Re-add with score 0 (unknown current score — but this is the safe
		// default; the Lua allocator uses ZINCRBY so the score will be corrected
		// on next allocation). Using ZAddNX to avoid overwriting if pod somehow
		// already got re-added by zombie cleanup.
		return client.ZAddNX(ctx, redisclient.PoolAvailableKey(poolKey), redis.Z{
			Score:  0,
			Member: podName,
		}).Err()
	}
	return client.SAdd(ctx, redisclient.PoolAvailableKey(poolKey), podName).Err()
}

// parseTier parses a tier string and returns the pool key and tier type flags.
//
// The tier value comes from the "voice:pod:tier:{pod}" Redis key, which stores:
//   - "merchant:merchant_name" for dedicated merchant pools
//   - "tier_name" (e.g. "gold", "standard") for regular pools
//
// Uses config.IsMerchantTier / ParseMerchantTier to detect merchant tiers via
// the "merchant:" prefix — no hardcoded tier name list.
func (d *Drainer) parseTier(tier string) (poolKey string, isMerchant bool, isShared bool) {
	if merchantID, ok := config.ParseMerchantTier(tier); ok {
		return merchantID, true, false
	}

	isShared = d.config.IsSharedTier(tier)
	return tier, false, isShared
}