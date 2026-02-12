package allocator

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"orchestration-api-go/internal/api/middleware"
	"orchestration-api-go/internal/config"
	"orchestration-api-go/internal/models"
)

const (
	// Redis key prefixes
	merchantPoolPrefix  = "voice:merchant:"
	poolAvailablePrefix = "voice:pool:"
	drainingPrefix      = "voice:pod:draining:"
	leasePrefix         = "voice:lease:"
	podInfoPrefix       = "voice:pod:"
	callInfoPrefix      = "voice:call:"
)

// Allocator implements tiered pod allocation with a configurable fallback chain.
type Allocator struct {
	redis  *redis.Client
	config *config.Config
	logger *zap.Logger
}

// NewAllocator creates a new allocator instance.
func NewAllocator(redis *redis.Client, cfg *config.Config, logger *zap.Logger) *Allocator {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Allocator{
		redis:  redis,
		config: cfg,
		logger: logger,
	}
}

// Allocate assigns a pod for the given call.
//
// It resolves a fallback chain (ordered list of pools to try) from the merchant
// config, then walks the chain until a pod is found. Each step tries either an
// exclusive pool (SPOP), shared pool (Lua ZINCRBY), or merchant dedicated pool
// depending on the tier type in config.
//
// Chain resolution:
//  1. If merchant has a dedicated pool → prepend "merchant:{pool}" to the chain
//  2. If merchant has a custom Fallback list → use it as the base
//  3. Otherwise → use the system-wide DefaultChain from TIER_CONFIG
func (a *Allocator) Allocate(ctx context.Context, callSID, merchantID, provider, flow, template string) (*models.AllocationResult, error) {
	if callSID == "" {
		return nil, ErrInvalidCallSID
	}

	// 1. Idempotency: check for existing allocation or acquire lock
	existing, err := CheckAndLockAllocation(ctx, a.redis, callSID)
	if err != nil {
		a.logger.Warn("failed to check existing allocation",
			zap.Error(err), zap.String("call_sid", callSID))
		// Fall through — worst case we allocate a duplicate, better than rejecting the call
	}
	if existing != nil {
		existing.WSURL = a.buildWSURL(existing.PodName, provider, flow, template)
		a.logger.Info("returning existing allocation",
			zap.String("call_sid", callSID),
			zap.String("pod_name", existing.PodName),
			zap.String("source_pool", existing.SourcePool))
		return existing, nil
	}

	// 2. Get merchant config (defaults to empty → uses DefaultChain)
	merchantConfig, err := GetMerchantConfig(ctx, a.redis, merchantID)
	if err != nil {
		a.logger.Warn("failed to get merchant config, using defaults",
			zap.Error(err), zap.String("merchant_id", merchantID))
		merchantConfig = models.MerchantConfig{}
	}

	// 3. Resolve the fallback chain
	chain := a.resolveFallbackChain(merchantConfig)

	// 4. Walk the chain, try each pool in order
	var podName, sourcePool string
	for _, step := range chain {
		podName, sourcePool, err = a.tryAllocateFromStep(ctx, step)
		if err == nil && podName != "" {
			break
		}
		// Step failed (pool empty or error) — try next
	}

	if podName == "" {
		a.logger.Warn("no pods available for allocation",
			zap.String("call_sid", callSID),
			zap.String("merchant_id", merchantID),
			zap.Strings("chain", chain))
		middleware.AllocationsTotal.WithLabelValues("", "no_pods").Inc()
		return nil, ErrNoPodsAvailable
	}

	// 5. Store allocation info in Redis
	now := time.Now()
	if err := a.storeAllocation(ctx, callSID, podName, sourcePool, merchantID, now); err != nil {
		a.logger.Error("failed to store allocation, returning pod to pool",
			zap.Error(err),
			zap.String("call_sid", callSID),
			zap.String("pod_name", podName),
			zap.String("source_pool", sourcePool))
		middleware.AllocationsTotal.WithLabelValues(sourcePool, "storage_error").Inc()
		a.returnPodToPool(ctx, podName, sourcePool)
		return nil, fmt.Errorf("allocation storage failed: %w", err)
	}

	result := &models.AllocationResult{
		PodName:     podName,
		WSURL:       a.buildWSURL(podName, provider, flow, template),
		SourcePool:  sourcePool,
		AllocatedAt: now,
		WasExisting: false,
	}

	a.logger.Info("pod allocated successfully",
		zap.String("call_sid", callSID),
		zap.String("pod_name", podName),
		zap.String("source_pool", sourcePool),
		zap.String("merchant_id", merchantID))

	middleware.AllocationsTotal.WithLabelValues(sourcePool, "success").Inc()
	middleware.ActiveCalls.Inc()
	return result, nil
}

// resolveFallbackChain builds the ordered list of pool steps to try.
//
// Each step is a string:
//   - "merchant:{id}"  → try the merchant's dedicated exclusive pool
//   - "{tier_name}"    → try the tier pool (exclusive or shared per config)
//
// Resolution:
//  1. Base = merchant's custom Fallback if set, otherwise config.DefaultChain.
//  2. If merchant has a dedicated Pool, prepend "merchant:{pool}" to the chain.
func (a *Allocator) resolveFallbackChain(mc models.MerchantConfig) []string {
	// Start with the base chain
	var base []string
	if len(mc.Fallback) > 0 {
		base = mc.Fallback
	} else {
		base = a.config.GetDefaultChain()
	}

	// Copy to avoid mutating config / merchant config slices
	chain := make([]string, 0, len(base)+1)

	// Prepend dedicated merchant pool if configured
	if mc.Pool != "" {
		chain = append(chain, "merchant:"+mc.Pool)
	}

	chain = append(chain, base...)
	return chain
}

// tryAllocateFromStep attempts allocation from a single step in the chain.
// Returns (podName, sourcePool, nil) on success, or ("", "", err) on failure.
func (a *Allocator) tryAllocateFromStep(ctx context.Context, step string) (podName, sourcePool string, err error) {
	// Merchant dedicated pool step
	if merchantID, ok := config.ParseMerchantTier(step); ok {
		poolKey := merchantPoolPrefix + merchantID + ":pods"
		podName, err = tryAllocateExclusive(ctx, a.redis, poolKey, drainingPrefix)
		if err != nil {
			return "", "", err
		}
		return podName, "merchant:" + merchantID, nil
	}

	// Regular tier pool — type determined by config
	tierCfg, known := a.config.GetTierConfig(step)
	if !known {
		a.logger.Debug("skipping unknown tier in fallback chain", zap.String("tier", step))
		return "", "", fmt.Errorf("unknown tier %q", step)
	}

	if tierCfg.Type == config.TierTypeShared {
		podName, err = tryAllocateShared(ctx, a.redis, step, tierCfg.MaxConcurrent)
	} else {
		poolKey := poolAvailablePrefix + step + ":available"
		podName, err = tryAllocateExclusive(ctx, a.redis, poolKey, drainingPrefix)
	}
	if err != nil {
		return "", "", err
	}
	return podName, "pool:" + step, nil
}

// buildWSURL constructs the WebSocket URL for a pod.
//
// Format: {baseURL}/ws/pod/{podName}/agent/voice/breeze-buddy/{provider}/callback/{template}[/v2]
//
// Nginx strips /ws/pod/{podName} and forwards the rest to the pod, matching
// the Python FastAPI mount (prefix="/agent/voice/breeze-buddy") plus the
// WebSocket handler routes (/{provider}/callback/{template}[/v2]).
func (a *Allocator) buildWSURL(podName, provider, flow, template string) string {
	baseURL := a.config.VoiceAgentBaseURL
	if baseURL == "" {
		baseURL = "wss://localhost:8081"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	if provider == "" {
		provider = "twilio"
	}
	if flow == "" {
		flow = "v2"
	}
	if template == "" {
		if provider == "exotel" {
			template = "template"
		} else {
			template = "order-confirmation"
		}
	}

	path := fmt.Sprintf("/agent/voice/breeze-buddy/%s/callback/%s", provider, template)
	if flow == "v2" {
		path += "/v2"
	}

	return fmt.Sprintf("%s/ws/pod/%s%s", baseURL, podName, path)
}

// storeAllocation stores the call→pod mapping and pod status in Redis.
// Overwrites the placeholder lock set by CheckAndLockAllocation.
func (a *Allocator) storeAllocation(ctx context.Context, callSID, podName, sourcePool, merchantID string, allocatedAt time.Time) error {
	callKey := callInfoPrefix + callSID
	ts := strconv.FormatInt(allocatedAt.Unix(), 10)

	// Store call info hash (overwrites lock placeholder)
	callData := map[string]interface{}{
		"pod_name":     podName,
		"source_pool":  sourcePool,
		"merchant_id":  merchantID,
		"allocated_at": ts,
	}
	if err := a.redis.HSet(ctx, callKey, callData).Err(); err != nil {
		return fmt.Errorf("failed to store call info: %w", err)
	}

	// Remove lock placeholder field
	a.redis.HDel(ctx, callKey, "_lock")

	// Set proper TTL (replaces the 30s lock TTL)
	if err := a.redis.Expire(ctx, callKey, a.config.CallInfoTTL).Err(); err != nil {
		a.logger.Warn("failed to set call info TTL", zap.Error(err))
	}

	// Update pod info
	podKey := podInfoPrefix + podName
	podData := map[string]string{
		"status":             "allocated",
		"allocated_call_sid": callSID,
		"allocated_at":       ts,
		"source_pool":        sourcePool,
	}
	if err := a.redis.HSet(ctx, podKey, podData).Err(); err != nil {
		a.logger.Warn("failed to update pod info",
			zap.Error(err), zap.String("pod_name", podName))
	}

	// Create lease — TTL from config (default 15min, set via LEASE_TTL env var).
	// Zombie cleanup uses lease presence to distinguish active pods from orphans,
	// so this MUST outlast the longest expected call. If it expires mid-call,
	// zombie cleanup adds the pod back to the available pool → double allocation.
	if err := a.redis.Set(ctx, leasePrefix+podName, callSID, a.config.LeaseTTL).Err(); err != nil {
		a.logger.Warn("failed to create lease",
			zap.Error(err), zap.String("pod_name", podName))
	}

	return nil
}

// returnPodToPool returns a pod to its source pool after a storage failure.
// Best-effort: zombie cleanup recovers the pod if this also fails.
func (a *Allocator) returnPodToPool(ctx context.Context, podName, sourcePool string) {
	parts := strings.SplitN(sourcePool, ":", 2)
	if len(parts) != 2 {
		a.logger.Error("cannot parse source_pool for recovery",
			zap.String("source_pool", sourcePool))
		return
	}

	poolType, poolID := parts[0], parts[1]

	switch poolType {
	case "merchant":
		if err := a.redis.SAdd(ctx, merchantPoolPrefix+poolID+":pods", podName).Err(); err != nil {
			a.logger.Error("failed to return pod to merchant pool",
				zap.Error(err), zap.String("pod", podName))
		}
	case "pool":
		if a.config.IsSharedTier(poolID) {
			if err := a.redis.ZIncrBy(ctx, poolAvailablePrefix+poolID+":available", -1, podName).Err(); err != nil {
				a.logger.Error("failed to return pod to shared pool",
					zap.Error(err), zap.String("pod", podName))
			}
		} else {
			if err := a.redis.SAdd(ctx, poolAvailablePrefix+poolID+":available", podName).Err(); err != nil {
				a.logger.Error("failed to return pod to exclusive pool",
					zap.Error(err), zap.String("pod", podName))
			}
		}
	default:
		a.logger.Error("unknown source_pool type for recovery",
			zap.String("source_pool", sourcePool))
	}
}
