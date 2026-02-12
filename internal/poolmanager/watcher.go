package poolmanager

import (
	"context"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"orchestration-api-go/internal/config"
)

// watchPods watches for pod events in Kubernetes and processes them.
func (m *Manager) watchPods(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		watcher, err := m.k8sClient.CoreV1().Pods(m.config.Namespace).Watch(ctx, metav1.ListOptions{
			LabelSelector: m.config.PodLabelSelector,
		})
		if err != nil {
			m.logger.Error("Failed to start watch", zap.Error(err))
			time.Sleep(5 * time.Second)
			continue
		}

		m.logger.Info("Started watching pods (Leader Mode)")
		m.handleWatchEvents(ctx, watcher)
		watcher.Stop()

		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
			// Brief pause before reconnecting
		}
	}
}

// handleWatchEvents processes events from the Kubernetes watch.
func (m *Manager) handleWatchEvents(ctx context.Context, watcher watch.Interface) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				m.logger.Info("Watch channel closed, reconnecting...")
				return
			}

			// Handle watch errors before type-asserting to *corev1.Pod,
			// because error events carry *metav1.Status, not a Pod object.
			if event.Type == watch.Error {
				m.logger.Error("Watch error event received",
					zap.Any("object", event.Object),
				)
				continue
			}

			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}

			switch event.Type {
			case watch.Added:
				m.handlePodAdded(ctx, pod)
			case watch.Modified:
				m.handlePodModified(ctx, pod)
			case watch.Deleted:
				m.handlePodDeleted(ctx, pod)
			}
		}
	}
}

// handlePodAdded handles pod creation events.
func (m *Manager) handlePodAdded(ctx context.Context, pod *corev1.Pod) {
	if !m.isPodReady(pod) || pod.Status.PodIP == "" {
		m.logger.Debug("Pod not ready yet, skipping",
			zap.String("pod", pod.Name),
			zap.String("phase", string(pod.Status.Phase)),
		)
		return
	}

	m.logger.Info("Pod added", zap.String("pod", pod.Name), zap.String("ip", pod.Status.PodIP))
	m.addPodToPool(ctx, pod)
}

// handlePodModified handles pod modification events.
func (m *Manager) handlePodModified(ctx context.Context, pod *corev1.Pod) {
	if pod.Name == "" || pod.Status.PodIP == "" {
		return
	}

	isReady := m.isPodReady(pod)
	isRegistered := m.isPodRegistered(ctx, pod.Name)

	if isReady && !isRegistered {
		m.logger.Info("Pod became ready", zap.String("pod", pod.Name))
		m.addPodToPool(ctx, pod)
	} else if !isReady && isRegistered {
		m.logger.Info("Pod no longer ready", zap.String("pod", pod.Name))
		m.removePodFromPool(ctx, pod)
	}
}

// handlePodDeleted handles pod deletion events.
func (m *Manager) handlePodDeleted(ctx context.Context, pod *corev1.Pod) {
	m.logger.Info("Pod deleted", zap.String("pod", pod.Name))
	m.removePodFromPool(ctx, pod)
}

// isPodReady checks if a pod is ready to receive traffic.
func (m *Manager) isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// isPodRegistered checks if a pod is registered in any pool.
func (m *Manager) isPodRegistered(ctx context.Context, podName string) bool {
	// Check all configured pools
	for tier, cfg := range m.config.GetParsedTierConfig() {
		if exists, err := m.redis.SIsMember(ctx, "voice:pool:"+tier+":assigned", podName).Result(); err != nil {
			m.logger.Error("Redis error checking pool", zap.String("tier", tier), zap.Error(err))
			return true // Fail safe: assume registered to avoid duplicates
		} else if exists {
			return true
		}

		// Non-shared tiers may have merchant pools
		if cfg.Type != config.TierTypeShared {
			if exists, err := m.redis.SIsMember(ctx, "voice:merchant:"+tier+":assigned", podName).Result(); err != nil {
				m.logger.Error("Redis error checking merchant pool", zap.String("tier", tier), zap.Error(err))
				return true
			} else if exists {
				return true
			}
		}
	}

	return false
}

// isPodEligible checks if a pod is eligible to be added to the available pool.
// On Redis errors, returns false (fail-safe: never add a potentially busy pod).
func (m *Manager) isPodEligible(ctx context.Context, podName string) bool {
	// Check if pod has an active lease
	hasLease, err := m.redis.Exists(ctx, "voice:lease:"+podName).Result()
	if err != nil {
		m.logger.Error("Redis error checking lease", zap.String("pod", podName), zap.Error(err))
		return false // Fail safe: assume not eligible
	}
	if hasLease > 0 {
		return false
	}

	// Check if pod is draining
	isDraining, err := m.redis.Exists(ctx, "voice:pod:draining:"+podName).Result()
	if err != nil {
		m.logger.Error("Redis error checking draining status", zap.String("pod", podName), zap.Error(err))
		return false // Fail safe: assume not eligible
	}
	if isDraining > 0 {
		return false
	}

	return true
}
