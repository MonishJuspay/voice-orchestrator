package poolmanager

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"orchestration-api-go/internal/api/middleware"
	"orchestration-api-go/internal/config"
)

// runZombieCleanup runs a periodic cleanup loop to recover orphaned pods.
// This handles scenarios where pods may have been removed from available pools
// but are still marked as assigned.
func (m *Manager) runZombieCleanup(ctx context.Context) {
	ticker := time.NewTicker(m.config.CleanupInterval)
	defer ticker.Stop()

	m.logger.Info("Zombie cleanup started", zap.Duration("interval", m.config.CleanupInterval))

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("Zombie cleanup stopped")
			return
		case <-ticker.C:
			m.cleanupZombies(ctx)
		}
	}
}

// cleanupZombies performs the actual zombie cleanup logic.
//
// It scans BOTH regular tier pools (voice:pool:{tier}:assigned) AND merchant
// pools (voice:merchant:{id}:assigned) to recover orphaned pods. Previously
// only regular pools were scanned, leaving merchant pods permanently lost.
//
// For EXCLUSIVE tiers: Checks lease → if absent and not in available pool, adds back.
// For SHARED tiers:    Checks if missing from ZSET, if so adds with score 0.
// For MERCHANT pools:  Same as exclusive (always exclusive SETs).
// Skips draining pods in all cases.
//
// Also updates PoolAvailablePods and PoolAssignedPods Prometheus gauges.
func (m *Manager) cleanupZombies(ctx context.Context) {
	// podEntry tracks a pod's pool type for recovery.
	type podEntry struct {
		tier       string // tier name (e.g. "gold") or merchant ID (e.g. "9shines")
		isMerchant bool
	}

	allAssignedPods := make(map[string]podEntry) // podName -> entry

	scanSet := func(key string, entry podEntry) {
		members, err := m.redis.SMembers(ctx, key).Result()
		if err != nil {
			m.logger.Warn("failed to scan assigned pods for zombie check",
				zap.String("key", key),
				zap.Error(err),
			)
			return
		}
		for _, member := range members {
			allAssignedPods[member] = entry
		}
	}

	// Scan regular tier pools AND merchant pools
	for tier, cfg := range m.config.GetParsedTierConfig() {
		scanSet("voice:pool:"+tier+":assigned", podEntry{tier: tier})

		// Non-shared tiers may have merchant pools sharing the same tier name
		if cfg.Type != config.TierTypeShared {
			scanSet("voice:merchant:"+tier+":assigned", podEntry{tier: tier, isMerchant: true})
		}
	}

	// --- Update Prometheus gauges while we have the data ---
	m.updatePoolMetrics(ctx)

	zombiesRecovered := 0

	for podName, entry := range allAssignedPods {
		// Skip draining pods
		isDraining, err := m.redis.Exists(ctx, "voice:pod:draining:"+podName).Result()
		if err != nil {
			m.logger.Warn("failed to check draining status in zombie cleanup",
				zap.String("pod", podName),
				zap.Error(err),
			)
			continue
		}
		if isDraining > 0 {
			continue
		}

		if entry.isMerchant {
			// MERCHANT POOL ZOMBIE CHECK — always exclusive SETs

			hasLease, err := m.redis.Exists(ctx, "voice:lease:"+podName).Result()
			if err != nil {
				m.logger.Warn("failed to check lease in merchant zombie cleanup",
					zap.String("pod", podName),
					zap.Error(err),
				)
				continue
			}
			if hasLease > 0 {
				continue // Active call, not a zombie
			}

			// Check if pod is in the merchant's available pool
			availKey := "voice:merchant:" + entry.tier + ":pods"
			isIn, err := m.redis.SIsMember(ctx, availKey, podName).Result()
			if err != nil {
				m.logger.Warn("failed to check merchant pool membership in zombie cleanup",
					zap.String("pod", podName),
					zap.String("key", availKey),
					zap.Error(err),
				)
				continue
			}
			if !isIn {
				if m.isPodEligible(ctx, podName) {
					m.redis.SAdd(ctx, availKey, podName)
					m.logger.Warn("Recovered merchant zombie pod",
						zap.String("pod", podName),
						zap.String("merchant", entry.tier),
					)
					zombiesRecovered++
					middleware.ZombiesRecoveredTotal.Inc()
					middleware.ActiveCalls.Dec()
				}
			}
			continue
		}

		// Determine tier type for regular pools
		isShared := false
		if cfg, ok := m.config.GetTierConfig(entry.tier); ok && cfg.Type == config.TierTypeShared {
			isShared = true
		}

	if isShared {
		// SHARED TIER ZOMBIE CHECK
		// For shared tiers, pods legitimately have leases while handling calls,
		// so isPodEligible (which checks for lease absence) would incorrectly
		// reject them. The draining check is already done above.
		// We only need to check if the pod is missing from the ZSET.

		availKey := "voice:pool:" + entry.tier + ":available"
		_, err := m.redis.ZScore(ctx, availKey, podName).Result()
		if err != nil && err != redis.Nil {
			// Real Redis error (connection blip, timeout, etc.) — skip this
			// pod. Do NOT treat it as missing; resetting a pod's score to 0
			// while it has active calls would cause an allocation storm.
			m.logger.Warn("failed to check shared pool membership in zombie cleanup",
				zap.String("pod", podName),
				zap.String("key", availKey),
				zap.Error(err),
			)
			continue
		}
		if err == redis.Nil {
			// Truly missing from ZSET — add back with score 0
			m.redis.ZAdd(ctx, availKey, redis.Z{
				Score:  0,
				Member: podName,
			})
			m.logger.Warn("Recovered shared zombie pod (missing from pool)",
				zap.String("pod", podName),
				zap.String("tier", entry.tier),
			)
			zombiesRecovered++
			middleware.ZombiesRecoveredTotal.Inc()
		}
		} else {
			// EXCLUSIVE TIER ZOMBIE CHECK

			hasLease, err := m.redis.Exists(ctx, "voice:lease:"+podName).Result()
			if err != nil {
				m.logger.Warn("failed to check lease in zombie cleanup",
					zap.String("pod", podName),
					zap.Error(err),
				)
				continue
			}
			if hasLease > 0 {
				continue // Pod is allocated, not a zombie
			}

			poolKey := "voice:pool:" + entry.tier + ":available"
			isIn, err := m.redis.SIsMember(ctx, poolKey, podName).Result()
			if err != nil {
				m.logger.Warn("failed to check pool membership in zombie cleanup",
					zap.String("pod", podName),
					zap.String("pool_key", poolKey),
					zap.Error(err),
				)
				continue
			}
			if !isIn {
				if m.isPodEligible(ctx, podName) {
					m.redis.SAdd(ctx, poolKey, podName)
					m.logger.Warn("Recovered exclusive zombie pod",
						zap.String("pod", podName),
						zap.String("tier", entry.tier),
					)
					zombiesRecovered++
					middleware.ZombiesRecoveredTotal.Inc()
					middleware.ActiveCalls.Dec()
				}
			}
		}
	}

	if zombiesRecovered > 0 {
		m.logger.Info("Zombie cleanup completed",
			zap.Int("assigned_pods_checked", len(allAssignedPods)),
			zap.Int("recovered", zombiesRecovered),
		)
	}
}

// updatePoolMetrics sets the PoolAvailablePods and PoolAssignedPods Prometheus
// gauges for every configured tier. Called once per zombie cleanup cycle.
func (m *Manager) updatePoolMetrics(ctx context.Context) {
	for tier, cfg := range m.config.GetParsedTierConfig() {
		// Assigned count
		assigned, err := m.redis.SCard(ctx, "voice:pool:"+tier+":assigned").Result()
		if err != nil {
			m.logger.Debug("failed to get assigned count for metrics",
				zap.String("tier", tier), zap.Error(err))
			continue
		}
		middleware.PoolAssignedPods.WithLabelValues(tier).Set(float64(assigned))

		// Available count — ZCARD for shared, SCARD for exclusive
		var available int64
		if cfg.Type == config.TierTypeShared {
			available, err = m.redis.ZCard(ctx, "voice:pool:"+tier+":available").Result()
		} else {
			available, err = m.redis.SCard(ctx, "voice:pool:"+tier+":available").Result()
		}
		if err != nil {
			m.logger.Debug("failed to get available count for metrics",
				zap.String("tier", tier), zap.Error(err))
			continue
		}
		middleware.PoolAvailablePods.WithLabelValues(tier).Set(float64(available))
	}
}
