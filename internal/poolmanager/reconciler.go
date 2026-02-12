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

	// Check for active call before deletion (Rolling Update / Pod Failure cleanup)
	activeCallSID, err := m.redis.HGet(ctx, "voice:pod:"+podName, "allocated_call_sid").Result()
	if err == nil && activeCallSID != "" {
		// Found an active call on this dying pod - clean it up
		if err := m.redis.Del(ctx, "voice:call:"+activeCallSID).Err(); err != nil {
			m.logger.Error("Failed to remove orphaned call mapping",
				zap.Error(err),
				zap.String("call_sid", activeCallSID),
				zap.String("pod", podName),
			)
		} else {
			m.logger.Info("Removed orphaned call mapping for dying pod",
				zap.String("call_sid", activeCallSID),
				zap.String("pod", podName),
			)
			middleware.ActiveCalls.Dec()
		}
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
