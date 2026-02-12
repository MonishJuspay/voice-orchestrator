package poolmanager

import (
	"context"

	"go.uber.org/zap"
	"orchestration-api-go/internal/config"
)

// autoAssignTier automatically assigns a tier to a new pod based on pool capacity.
//
// Priority order:
//  1. If already assigned (voice:pod:tier:{name}), return the existing tier.
//  2. Merchant pools (tiers in config with "merchant:" prefix convention are
//     separate — here we check tiers in ParsedTierConfig that are NOT in
//     DefaultChain, treating them as merchant pools).
//  3. Walk DefaultChain in order: for each tier, check if the pool has room
//     (current assigned < target). First tier with capacity wins.
//  4. If all pools are at capacity, assign to the last tier in DefaultChain.
func (m *Manager) autoAssignTier(ctx context.Context, podName string) (string, bool) {
	// 1. Check if already assigned
	if existingTier, err := m.redis.Get(ctx, "voice:pod:tier:"+podName).Result(); err == nil && existingTier != "" {
		if merchantID, ok := config.ParseMerchantTier(existingTier); ok {
			return merchantID, true
		}
		return existingTier, false
	}

	// 2. Check merchant pools (tiers in config not present in DefaultChain)
	defaultChain := m.config.GetDefaultChain()
	tierConfig := m.config.GetParsedTierConfig()

	chainSet := make(map[string]bool, len(defaultChain))
	for _, t := range defaultChain {
		chainSet[t] = true
	}
	for tier, cfg := range tierConfig {
		if chainSet[tier] {
			continue // Part of the regular chain, handled below
		}
		if cfg.Type == config.TierTypeShared {
			continue // Shared pools are not merchant pools
		}

		assignedKey := "voice:merchant:" + tier + ":assigned"
		currentAssigned, err := m.redis.SCard(ctx, assignedKey).Result()
		if err != nil {
			m.logger.Warn("failed to check merchant pool capacity, skipping",
				zap.String("tier", tier),
				zap.Error(err),
			)
			continue
		}
		if currentAssigned < int64(cfg.Target) {
			return tier, true
		}
	}

	// 3. Walk DefaultChain in order
	for _, tier := range defaultChain {
		cfg, ok := tierConfig[tier]
		if !ok {
			continue
		}

		assignedKey := "voice:pool:" + tier + ":assigned"
		current, err := m.redis.SCard(ctx, assignedKey).Result()
		if err != nil {
			m.logger.Warn("failed to check pool capacity, skipping",
				zap.String("tier", tier),
				zap.Error(err),
			)
			continue
		}
		if current < int64(cfg.Target) {
			return tier, false
		}
	}

	// 4. All pools at capacity — fall back to last tier in DefaultChain
	if len(defaultChain) > 0 {
		fallback := defaultChain[len(defaultChain)-1]
		m.logger.Debug("All pools at capacity, defaulting to last chain tier",
			zap.String("pod", podName),
			zap.String("tier", fallback),
		)
		return fallback, false
	}

	// Absolute fallback — shouldn't happen if config is valid
	m.logger.Warn("No tiers configured, cannot assign pod",
		zap.String("pod", podName),
	)
	return "standard", false
}


