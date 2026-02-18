package poolmanager

import (
	"context"
	"encoding/json"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"orchestration-api-go/internal/api/middleware"
	"orchestration-api-go/internal/config"
	"orchestration-api-go/internal/models"
)

// syncAllPods performs a full reconciliation between Kubernetes and Redis.
// It lists all pods from Kubernetes, compares with Redis state, and fixes discrepancies.
func (m *Manager) syncAllPods(ctx context.Context) error {
	// List all pods from Kubernetes
	k8sPods, err := m.k8sClient.CoreV1().Pods(m.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: m.config.PodLabelSelector,
	})
	if err != nil {
		return err
	}

	// Build map of K8s pods by name
	k8sPodMap := make(map[string]*corev1.Pod)
	for i := range k8sPods.Items {
		pod := &k8sPods.Items[i]
		k8sPodMap[pod.Name] = pod
	}

	// Get all pods registered in Redis
	allRedisPods := m.getAllRedisPods(ctx)

	// 1. Add/update pods that exist in K8s
	for name, pod := range k8sPodMap {
		if m.isPodReady(pod) && pod.Status.PodIP != "" {
			m.addPodToPool(ctx, pod)
		} else {
			m.removePodFromPool(ctx, pod)
		}
		delete(allRedisPods, name)
	}

	// 2. Remove ghost pods (in Redis but not in K8s)
	for ghostPodName := range allRedisPods {
		m.logger.Warn("Found ghost pod in Redis", zap.String("pod", ghostPodName))
		dummyPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: ghostPodName},
		}
		m.removePodFromPool(ctx, dummyPod)
	}

	m.logger.Info("Reconciliation complete",
		zap.Int("k8s_pods", len(k8sPods.Items)),
		zap.Int("ghost_pods_removed", len(allRedisPods)),
	)

	// 3. Rebalance tiers if any are over/under target
	m.rebalanceTiers(ctx)

	return nil
}

// getAllRedisPods returns a map of all pods registered in Redis pools.
func (m *Manager) getAllRedisPods(ctx context.Context) map[string]bool {
	pods := make(map[string]bool)

	scanSet := func(key string) {
		members, err := m.redis.SMembers(ctx, key).Result()
		if err != nil {
			m.logger.Warn("failed to scan Redis set for pods",
				zap.String("key", key),
				zap.Error(err),
			)
			return
		}
		for _, member := range members {
			pods[member] = true
		}
	}

	// Scan all pools from config — covers every configured tier
	for tier, cfg := range m.config.GetParsedTierConfig() {
		scanSet("voice:pool:" + tier + ":assigned")
		// Non-shared tiers may also have merchant pools
		if cfg.Type != config.TierTypeShared {
			scanSet("voice:merchant:" + tier + ":assigned")
		}
	}

	return pods
}

// addPodToPool adds a pod to the appropriate Redis pool (SET or ZSET).
func (m *Manager) addPodToPool(ctx context.Context, pod *corev1.Pod) {
	podName := pod.Name

	// Check if already assigned to a tier
	existingTier, err := m.redis.Get(ctx, "voice:pod:tier:"+podName).Result()
	if err == nil && existingTier != "" {
		needsReassign := false

		if merchantID, ok := config.ParseMerchantTier(existingTier); ok {
			// Merchant tier — verify the merchant still exists in tier config.
			// If the merchant was removed from config, re-assign the pod.
			if !m.config.IsKnownTier(merchantID) {
				m.logger.Warn("Pod in stale merchant tier (removed from config), re-assigning",
					zap.String("pod", podName),
					zap.String("old_tier", existingTier))
				needsReassign = true
			}
		} else if !m.config.IsKnownTier(existingTier) {
			// Regular tier removed from config — re-assign
			m.logger.Warn("Pod in unknown/deleted tier, re-assigning",
				zap.String("pod", podName),
				zap.String("old_tier", existingTier))
			needsReassign = true
		}

		if needsReassign {
			// Clean up stale pool memberships before re-assignment
			if merchantID, ok := config.ParseMerchantTier(existingTier); ok {
				m.redis.SRem(ctx, "voice:merchant:"+merchantID+":assigned", podName)
				m.redis.SRem(ctx, "voice:merchant:"+merchantID+":pods", podName)
			} else {
				m.redis.SRem(ctx, "voice:pool:"+existingTier+":assigned", podName)
				m.redis.SRem(ctx, "voice:pool:"+existingTier+":available", podName)
			}
			// Clean up the stale tier key so autoAssignTier starts fresh
			m.redis.Del(ctx, "voice:pod:tier:"+podName)
		} else {
			m.ensurePodInPool(ctx, podName, existingTier)
			return
		}
	}

	// Auto-assign tier
	assignedTier, isMerchant := m.autoAssignTier(ctx, podName)

	// Store metadata
	metadata := models.PodMetadata{Tier: assignedTier, Name: pod.Name}
	metadataJSON, _ := json.Marshal(metadata)
	m.redis.HSet(ctx, "voice:pod:metadata", podName, string(metadataJSON))

	// Determine if Shared or Exclusive
	isShared := false
	if cfg, ok := m.config.GetTierConfig(assignedTier); ok && cfg.Type == config.TierTypeShared {
		isShared = true
	}

	if isMerchant {
		// Merchant pools are always exclusive for now
		m.redis.SAdd(ctx, "voice:merchant:"+assignedTier+":assigned", podName)
		m.redis.Set(ctx, "voice:pod:tier:"+podName, "merchant:"+assignedTier, 0)

		if m.isPodEligible(ctx, podName) {
			m.redis.SAdd(ctx, "voice:merchant:"+assignedTier+":pods", podName)
		}

		m.logger.Info("Pod added to merchant pool",
			zap.String("pod", pod.Name),
			zap.String("merchant", assignedTier),
		)
	} else {
		// Generic Pool
		m.redis.SAdd(ctx, "voice:pool:"+assignedTier+":assigned", podName)
		m.redis.Set(ctx, "voice:pod:tier:"+podName, assignedTier, 0)

		if m.isPodEligible(ctx, podName) {
			if isShared {
				// SHARED TIER uses ZSET (Score 0)
				// ZAddNX ensures we don't reset score if it already exists
				m.redis.ZAddNX(ctx, "voice:pool:"+assignedTier+":available", redis.Z{
					Score:  0,
					Member: podName,
				})
				m.logger.Info("Pod added to SHARED pool",
					zap.String("pod", pod.Name),
					zap.String("tier", assignedTier),
				)
			} else {
				// EXCLUSIVE TIER uses SET
				m.redis.SAdd(ctx, "voice:pool:"+assignedTier+":available", podName)
				m.logger.Info("Pod added to EXCLUSIVE pool",
					zap.String("pod", pod.Name),
					zap.String("tier", assignedTier),
				)
			}
		}
	}
}

// removePodFromPool removes a pod from all Redis pools.
func (m *Manager) removePodFromPool(ctx context.Context, pod *corev1.Pod) {
	podName := pod.Name
	tier := m.getPodTier(ctx, podName)

	// Remove from all configured pools using type-aware commands
	// (ZRem for shared, SRem for exclusive) to avoid WRONGTYPE errors.
	for t, cfg := range m.config.GetParsedTierConfig() {
		m.redis.SRem(ctx, "voice:pool:"+t+":assigned", podName)
		m.redis.SRem(ctx, "voice:merchant:"+t+":assigned", podName)

		if cfg.Type == config.TierTypeShared {
			m.redis.ZRem(ctx, "voice:pool:"+t+":available", podName)
		} else {
			m.redis.SRem(ctx, "voice:pool:"+t+":available", podName)
			m.redis.SRem(ctx, "voice:merchant:"+t+":pods", podName)
		}
	}

	// Clean up ALL active calls on this dying pod.
	// The pod's calls SET tracks every concurrent call (critical for shared pods
	// that handle multiple calls simultaneously).
	podCallsKey := "voice:pod:" + podName + ":calls"
	activeCallSIDs, err := m.redis.SMembers(ctx, podCallsKey).Result()
	if err != nil {
		m.logger.Error("Failed to read pod calls set",
			zap.Error(err), zap.String("pod", podName))
	}

	for _, callSID := range activeCallSIDs {
		if err := m.redis.Del(ctx, "voice:call:"+callSID).Err(); err != nil {
			m.logger.Error("Failed to remove orphaned call mapping",
				zap.Error(err),
				zap.String("call_sid", callSID),
				zap.String("pod", podName),
			)
		} else {
			m.logger.Info("Removed orphaned call mapping for dying pod",
				zap.String("call_sid", callSID),
				zap.String("pod", podName),
			)
			middleware.ActiveCalls.Dec()
		}
	}
	if len(activeCallSIDs) > 0 {
		m.redis.Del(ctx, podCallsKey)
	}

	// Clean up metadata and leases
	m.redis.HDel(ctx, "voice:pod:metadata", podName)
	m.redis.Del(ctx, "voice:pod:"+podName) // Also delete the pod info hash itself
	m.redis.Del(ctx, "voice:pod:tier:"+podName)
	m.redis.Del(ctx, "voice:lease:"+podName)
	m.redis.Del(ctx, "voice:pod:draining:"+podName) // Ensure draining key is removed

	m.logger.Info("Pod removed",
		zap.String("pod", pod.Name),
		zap.String("tier", tier),
	)
}

// ensurePodInPool ensures a pod is in its assigned pool (handling Sets vs ZSets).
func (m *Manager) ensurePodInPool(ctx context.Context, podName, tier string) {
	if merchantID, ok := config.ParseMerchantTier(tier); ok {
		m.redis.SAdd(ctx, "voice:merchant:"+merchantID+":assigned", podName)
		if m.isPodEligible(ctx, podName) {
			m.redis.SAdd(ctx, "voice:merchant:"+merchantID+":pods", podName)
		}
		return
	}

	m.redis.SAdd(ctx, "voice:pool:"+tier+":assigned", podName)

	isShared := false
	if cfg, ok := m.config.GetTierConfig(tier); ok && cfg.Type == config.TierTypeShared {
		isShared = true
	}

	if m.isPodEligible(ctx, podName) {
		if isShared {
			m.redis.ZAddNX(ctx, "voice:pool:"+tier+":available", redis.Z{
				Score:  0,
				Member: podName,
			})
		} else {
			m.redis.SAdd(ctx, "voice:pool:"+tier+":available", podName)
		}
	}
}

// rebalanceTiers moves idle pods from over-target tiers to under-target tiers.
//
// When tier targets change in Redis config (e.g. gold 3→4, basic 3→2), existing
// pods stay in their current tier because autoAssignTier only runs for NEW pods.
// This method detects the imbalance and moves idle pods to satisfy the new targets.
//
// Safety guarantees:
//   - Only moves pods with ZERO active calls (idle)
//   - Skips pods that are draining or have active leases
//   - Uses an atomic Lua script for shared pods to prevent race conditions
//     where a call could be allocated between the idle check and the removal
func (m *Manager) rebalanceTiers(ctx context.Context) {
	tierConfig := m.config.GetParsedTierConfig()
	defaultChain := m.config.GetDefaultChain()

	// Pass 1: rebalance default chain tiers.
	// Merchant pools are handled in a second pass (rebalanceMerchantTiers).
	type tierDelta struct {
		name  string
		delta int64 // positive = over-target (has excess), negative = under-target (needs pods)
	}

	var overTiers, underTiers []tierDelta
	for _, tier := range defaultChain {
		cfg, ok := tierConfig[tier]
		if !ok {
			continue
		}
		assigned, err := m.redis.SCard(ctx, "voice:pool:"+tier+":assigned").Result()
		if err != nil {
			m.logger.Warn("rebalance: failed to get assigned count",
				zap.String("tier", tier), zap.Error(err))
			continue
		}
		diff := assigned - int64(cfg.Target)
		if diff > 0 {
			overTiers = append(overTiers, tierDelta{name: tier, delta: diff})
		} else if diff < 0 {
			underTiers = append(underTiers, tierDelta{name: tier, delta: -diff}) // store as positive "need"
		}
	}

	if len(overTiers) == 0 || len(underTiers) == 0 {
		// Nothing to rebalance in default chain, but merchant tiers may still need it.
		m.rebalanceMerchantTiers(ctx, tierConfig, defaultChain)
		return
	}

	// Lua script: atomically check ZSCORE==0 and ZREM for shared pods.
	// Returns 1 if the pod was idle and removed, 0 otherwise.
	atomicSharedRemove := redis.NewScript(`
local score = redis.call('ZSCORE', KEYS[1], ARGV[1])
if score and tonumber(score) == 0 then
    redis.call('ZREM', KEYS[1], ARGV[1])
    return 1
end
return 0
`)

	moved := 0
	for i := range overTiers {
		ot := &overTiers[i]
		if ot.delta <= 0 {
			continue
		}

		otCfg := tierConfig[ot.name]
		isShared := otCfg.Type == config.TierTypeShared

		// Get candidate pods in this over-target tier
		candidates, err := m.redis.SMembers(ctx, "voice:pool:"+ot.name+":assigned").Result()
		if err != nil {
			m.logger.Warn("rebalance: failed to list assigned pods",
				zap.String("tier", ot.name), zap.Error(err))
			continue
		}

		for _, podName := range candidates {
			if ot.delta <= 0 {
				break // This tier is no longer over-target
			}

			// Find an under-target tier that still needs pods
			var target *tierDelta
			for j := range underTiers {
				if underTiers[j].delta > 0 {
					target = &underTiers[j]
					break
				}
			}
			if target == nil {
				break // All under-target tiers are satisfied
			}

			// Skip pods that are draining or have active leases
			if !m.isPodEligible(ctx, podName) {
				continue
			}

			// Check if pod is idle and atomically remove from available pool
			if isShared {
				// Shared: atomic ZSCORE==0 check + ZREM
				result, err := atomicSharedRemove.Run(ctx, m.redis,
					[]string{"voice:pool:" + ot.name + ":available"},
					podName,
				).Int64()
				if err != nil {
					m.logger.Warn("rebalance: Lua script failed",
						zap.String("pod", podName), zap.Error(err))
					continue
				}
				if result == 0 {
					continue // Pod has active calls, skip
				}
				// Pod was atomically removed from available ZSET
			} else {
				// Exclusive: try SREM from available SET.
				// If the pod is NOT in available, it's allocated (has an active call).
				removed, err := m.redis.SRem(ctx, "voice:pool:"+ot.name+":available", podName).Result()
				if err != nil {
					m.logger.Warn("rebalance: failed to remove from exclusive available",
						zap.String("pod", podName), zap.Error(err))
					continue
				}
				if removed == 0 {
					continue // Pod is allocated, skip
				}
				// Pod was removed from available SET
			}

			// Pod is confirmed idle and removed from old available pool.
			// Now complete the move:

			// 1. Remove from old tier's assigned SET
			m.redis.SRem(ctx, "voice:pool:"+ot.name+":assigned", podName)

			// 2. Delete old tier key
			m.redis.Del(ctx, "voice:pod:tier:"+podName)

			// 3. Assign to new tier
			targetCfg := tierConfig[target.name]
			m.redis.Set(ctx, "voice:pod:tier:"+podName, target.name, 0)
			m.redis.SAdd(ctx, "voice:pool:"+target.name+":assigned", podName)

			if targetCfg.Type == config.TierTypeShared {
				m.redis.ZAdd(ctx, "voice:pool:"+target.name+":available", redis.Z{
					Score:  0,
					Member: podName,
				})
			} else {
				m.redis.SAdd(ctx, "voice:pool:"+target.name+":available", podName)
			}

			// 4. Update metadata
			metadataJSON, err := m.redis.HGet(ctx, "voice:pod:metadata", podName).Result()
			if err == nil {
				var metadata models.PodMetadata
				if json.Unmarshal([]byte(metadataJSON), &metadata) == nil {
					metadata.Tier = target.name
					newJSON, _ := json.Marshal(metadata)
					m.redis.HSet(ctx, "voice:pod:metadata", podName, string(newJSON))
				}
			}

			m.logger.Info("Rebalanced pod",
				zap.String("pod", podName),
				zap.String("from_tier", ot.name),
				zap.String("to_tier", target.name),
			)

			ot.delta--
			target.delta--
			moved++
		}
	}

	if moved > 0 {
		m.logger.Info("Default chain rebalancing complete", zap.Int("pods_moved", moved))
	}

	// -----------------------------------------------------------------------
	// Second pass: rebalance merchant tiers.
	//
	// Merchant tiers are those in the config but NOT in the default chain.
	// If a merchant tier is over-target, donate excess idle pods to any
	// under-target tier — merchant tiers first (dedicated commitments),
	// then default chain tiers.
	// -----------------------------------------------------------------------
	m.rebalanceMerchantTiers(ctx, tierConfig, defaultChain)
}

// rebalanceMerchantTiers handles rebalancing involving merchant tiers.
//
// Two directions:
//  1. Merchant over-target → donate excess to under-target merchant or default chain tiers
//  2. Default chain over-target → donate excess to under-target merchant tiers
//
// This covers the cases that pass 1 (default→default only) cannot handle.
func (m *Manager) rebalanceMerchantTiers(ctx context.Context, tierConfig map[string]config.TierConfig, defaultChain []string) {
	chainSet := make(map[string]bool, len(defaultChain))
	for _, t := range defaultChain {
		chainSet[t] = true
	}

	// Identify merchant tiers (in config but not in default chain, exclusive only)
	type tierDelta struct {
		name       string
		delta      int64
		isMerchant bool
	}

	var overMerchant []tierDelta
	var underMerchant []tierDelta

	for tier, cfg := range tierConfig {
		if chainSet[tier] {
			continue // Part of default chain, handled separately
		}
		if cfg.Type == config.TierTypeShared {
			continue // Shared pools are not merchant pools
		}

		assigned, err := m.redis.SCard(ctx, "voice:merchant:"+tier+":assigned").Result()
		if err != nil {
			m.logger.Warn("rebalanceMerchant: failed to get assigned count",
				zap.String("merchant", tier), zap.Error(err))
			continue
		}
		diff := assigned - int64(cfg.Target)
		if diff > 0 {
			overMerchant = append(overMerchant, tierDelta{name: tier, delta: diff, isMerchant: true})
		} else if diff < 0 {
			underMerchant = append(underMerchant, tierDelta{name: tier, delta: -diff, isMerchant: true})
		}
	}

	// Also compute default chain tier deltas (fresh from Redis, reflecting pass 1 moves)
	var overChain []tierDelta
	var underChain []tierDelta
	for _, tier := range defaultChain {
		cfg, ok := tierConfig[tier]
		if !ok {
			continue
		}
		assigned, err := m.redis.SCard(ctx, "voice:pool:"+tier+":assigned").Result()
		if err != nil {
			continue
		}
		diff := assigned - int64(cfg.Target)
		if diff > 0 {
			overChain = append(overChain, tierDelta{name: tier, delta: diff, isMerchant: false})
		} else if diff < 0 {
			underChain = append(underChain, tierDelta{name: tier, delta: -diff, isMerchant: false})
		}
	}

	// Build combined donor and receiver lists
	// Donors: over-target merchant tiers + over-target default chain tiers (for merchant receivers)
	// Receivers: under-target merchant tiers + under-target default chain tiers
	//
	// Note: default→default is already handled by pass 1, so we only need:
	//   - merchant donors → any receiver (merchant or default chain)
	//   - default chain donors → merchant receivers only

	hasWork := false
	if len(overMerchant) > 0 && (len(underMerchant) > 0 || len(underChain) > 0) {
		hasWork = true
	}
	if len(overChain) > 0 && len(underMerchant) > 0 {
		hasWork = true
	}
	if !hasWork {
		return
	}

	// Lua script: atomically check ZSCORE==0 and ZREM for shared pods.
	atomicSharedRemove := redis.NewScript(`
local score = redis.call('ZSCORE', KEYS[1], ARGV[1])
if score and tonumber(score) == 0 then
    redis.call('ZREM', KEYS[1], ARGV[1])
    return 1
end
return 0
`)

	moved := 0

	// movePod handles the destination side of a rebalance move.
	// The caller must have already removed the pod from its source available pool.
	movePod := func(podName string, target *tierDelta) {
		if target.isMerchant {
			m.redis.Set(ctx, "voice:pod:tier:"+podName, "merchant:"+target.name, 0)
			m.redis.SAdd(ctx, "voice:merchant:"+target.name+":assigned", podName)
			m.redis.SAdd(ctx, "voice:merchant:"+target.name+":pods", podName)
		} else {
			targetCfg := tierConfig[target.name]
			m.redis.Set(ctx, "voice:pod:tier:"+podName, target.name, 0)
			m.redis.SAdd(ctx, "voice:pool:"+target.name+":assigned", podName)

			if targetCfg.Type == config.TierTypeShared {
				m.redis.ZAdd(ctx, "voice:pool:"+target.name+":available", redis.Z{
					Score:  0,
					Member: podName,
				})
			} else {
				m.redis.SAdd(ctx, "voice:pool:"+target.name+":available", podName)
			}
		}

		// Update metadata
		metadataJSON, err := m.redis.HGet(ctx, "voice:pod:metadata", podName).Result()
		if err == nil {
			var metadata models.PodMetadata
			if json.Unmarshal([]byte(metadataJSON), &metadata) == nil {
				if target.isMerchant {
					metadata.Tier = "merchant:" + target.name
				} else {
					metadata.Tier = target.name
				}
				newJSON, _ := json.Marshal(metadata)
				m.redis.HSet(ctx, "voice:pod:metadata", podName, string(newJSON))
			}
		}
		target.delta--
	}

	// findReceiver finds the first under-target tier to receive a pod.
	// Priority: merchant tiers first, then default chain tiers.
	// onlyMerchant restricts to merchant receivers (for default chain donors).
	findReceiver := func(onlyMerchant bool) *tierDelta {
		for i := range underMerchant {
			if underMerchant[i].delta > 0 {
				return &underMerchant[i]
			}
		}
		if !onlyMerchant {
			for i := range underChain {
				if underChain[i].delta > 0 {
					return &underChain[i]
				}
			}
		}
		return nil
	}

	// Direction 1: over-target merchant → any under-target tier
	for i := range overMerchant {
		om := &overMerchant[i]
		if om.delta <= 0 {
			continue
		}

		candidates, err := m.redis.SMembers(ctx, "voice:merchant:"+om.name+":assigned").Result()
		if err != nil {
			m.logger.Warn("rebalanceMerchant: failed to list assigned pods",
				zap.String("merchant", om.name), zap.Error(err))
			continue
		}

		for _, podName := range candidates {
			if om.delta <= 0 {
				break
			}
			target := findReceiver(false)
			if target == nil {
				break
			}
			if !m.isPodEligible(ctx, podName) {
				continue
			}

			// Merchant pools are exclusive — SREM from available set
			removed, err := m.redis.SRem(ctx, "voice:merchant:"+om.name+":pods", podName).Result()
			if err != nil {
				m.logger.Warn("rebalanceMerchant: failed to remove from merchant available",
					zap.String("pod", podName), zap.Error(err))
				continue
			}
			if removed == 0 {
				continue // Pod has active call, skip
			}

			m.redis.SRem(ctx, "voice:merchant:"+om.name+":assigned", podName)
			m.redis.Del(ctx, "voice:pod:tier:"+podName)
			movePod(podName, target)

			m.logger.Info("Rebalanced pod (merchant donor)",
				zap.String("pod", podName),
				zap.String("from", "merchant:"+om.name),
				zap.String("to", target.name),
				zap.Bool("to_merchant", target.isMerchant),
			)
			om.delta--
			moved++
		}
	}

	// Direction 2: over-target default chain → under-target merchant
	for i := range overChain {
		oc := &overChain[i]
		if oc.delta <= 0 {
			continue
		}

		ocCfg := tierConfig[oc.name]
		isShared := ocCfg.Type == config.TierTypeShared

		candidates, err := m.redis.SMembers(ctx, "voice:pool:"+oc.name+":assigned").Result()
		if err != nil {
			m.logger.Warn("rebalanceMerchant: failed to list assigned pods",
				zap.String("tier", oc.name), zap.Error(err))
			continue
		}

		for _, podName := range candidates {
			if oc.delta <= 0 {
				break
			}
			target := findReceiver(true) // Only merchant receivers
			if target == nil {
				break
			}
			if !m.isPodEligible(ctx, podName) {
				continue
			}

			// Remove from source available pool
			if isShared {
				result, err := atomicSharedRemove.Run(ctx, m.redis,
					[]string{"voice:pool:" + oc.name + ":available"},
					podName,
				).Int64()
				if err != nil {
					m.logger.Warn("rebalanceMerchant: Lua script failed",
						zap.String("pod", podName), zap.Error(err))
					continue
				}
				if result == 0 {
					continue // Pod has active calls, skip
				}
			} else {
				removed, err := m.redis.SRem(ctx, "voice:pool:"+oc.name+":available", podName).Result()
				if err != nil {
					m.logger.Warn("rebalanceMerchant: failed to remove from available",
						zap.String("pod", podName), zap.Error(err))
					continue
				}
				if removed == 0 {
					continue // Pod is allocated, skip
				}
			}

			m.redis.SRem(ctx, "voice:pool:"+oc.name+":assigned", podName)
			m.redis.Del(ctx, "voice:pod:tier:"+podName)
			movePod(podName, target)

			m.logger.Info("Rebalanced pod (chain donor to merchant)",
				zap.String("pod", podName),
				zap.String("from", oc.name),
				zap.String("to", "merchant:"+target.name),
			)
			oc.delta--
			moved++
		}
	}

	if moved > 0 {
		m.logger.Info("Merchant tier rebalancing complete", zap.Int("pods_moved", moved))
	}
}

// getPodTier retrieves the tier for a pod from Redis.
// Falls back to the last tier in DefaultChain if tier info is unavailable.
func (m *Manager) getPodTier(ctx context.Context, podName string) string {
	// Check direct tier key
	if tier, err := m.redis.Get(ctx, "voice:pod:tier:"+podName).Result(); err == nil && tier != "" {
		return tier
	}

	// Fallback to metadata
	metadataJSON, err := m.redis.HGet(ctx, "voice:pod:metadata", podName).Result()
	if err == nil {
		var metadata models.PodMetadata
		if err := json.Unmarshal([]byte(metadataJSON), &metadata); err == nil && metadata.Tier != "" {
			return metadata.Tier
		}
	}

	// Last resort: last tier in DefaultChain
	chain := m.config.GetDefaultChain()
	if len(chain) > 0 {
		return chain[len(chain)-1]
	}
	return "unknown"
}
