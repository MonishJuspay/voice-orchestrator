// Package poolmanager implements the LEADER-ONLY component that watches
// Kubernetes pods and manages pod pools in Redis.
package poolmanager

import (
	"context"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"orchestration-api-go/internal/api/middleware"
	"orchestration-api-go/internal/config"
)

// Manager is the LEADER-ONLY component that watches Kubernetes pods
// and manages pod pools in Redis.
type Manager struct {
	k8sClient *kubernetes.Clientset
	redis     *redis.Client
	config    *config.Config
	logger    *zap.Logger
	isLeader  atomic.Bool
}

// NewManager creates a new pool manager instance.
func NewManager(k8sClient *kubernetes.Clientset, redisClient *redis.Client, cfg *config.Config, logger *zap.Logger) *Manager {
	if logger == nil {
		logger = zap.NewNop()
	}

	return &Manager{
		k8sClient: k8sClient,
		redis:     redisClient,
		config:    cfg,
		logger:    logger,
	}
}

// IsLeader returns true if this instance is currently the leader.
func (m *Manager) IsLeader() bool {
	return m.isLeader.Load()
}

// Run starts the manager with leader election.
// This method blocks until the context is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	if !m.config.LeaderElectionEnabled {
		m.logger.Info("Leader election disabled, running as leader directly")
		m.isLeader.Store(true)
		middleware.LeaderStatus.Set(1)
		return m.runLeaderWorkload(ctx)
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      m.config.LeaderElectionLockName,
			Namespace: m.config.LeaderElectionNamespace,
		},
		Client: m.k8sClient.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: m.config.PodName,
		},
	}

	m.logger.Info("Starting leader election",
		zap.String("lock_name", m.config.LeaderElectionLockName),
		zap.String("namespace", m.config.LeaderElectionNamespace),
		zap.String("pod_name", m.config.PodName),
	)

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   m.config.LeaderElectionDuration,
		RenewDeadline:   m.config.LeaderElectionRenewDeadline,
		RetryPeriod:     m.config.LeaderElectionRetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				m.logger.Info("🔥 I am the LEADER. Starting pool management...")
				m.isLeader.Store(true)
				middleware.LeaderStatus.Set(1)
				if err := m.runLeaderWorkload(ctx); err != nil {
					m.logger.Error("Leader workload failed", zap.Error(err))
				}
			},
			OnStoppedLeading: func() {
				m.logger.Info("🛑 Lost leadership. Stopping pool management.")
				m.isLeader.Store(false)
				middleware.LeaderStatus.Set(0)
				// Send signal to trigger graceful shutdown
				process, _ := os.FindProcess(os.Getpid())
				if process != nil {
					process.Signal(syscall.SIGTERM)
				}
			},
			OnNewLeader: func(identity string) {
				if identity == m.config.PodName {
					return
				}
				m.logger.Info("Leader elected", zap.String("leader", identity))
			},
		},
	})

	return nil
}

// runLeaderWorkload is the core logic that ONLY the leader runs.
func (m *Manager) runLeaderWorkload(ctx context.Context) error {
	m.logger.Info("Starting Safe Reconciliation...")

	// Perform initial full sync
	if err := m.syncAllPods(ctx); err != nil {
		m.logger.Error("Initial sync failed", zap.Error(err))
		// Continue anyway - will retry via watch
	}

	// Start periodic sync ticker (recover from Redis flush/data loss)
	syncTicker := time.NewTicker(1 * time.Minute)
	defer syncTicker.Stop()

	// Start background goroutines with crash recovery (auto-restart on panic)
	go m.safeGo(ctx, "watchPods", func() { m.watchPods(ctx) })
	go m.safeGo(ctx, "zombieCleanup", func() { m.runZombieCleanup(ctx) })

	// Main loop
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-syncTicker.C:
			if err := m.syncAllPods(ctx); err != nil {
				m.logger.Error("Periodic sync failed", zap.Error(err))
			}
		}
	}
}

// safeGo wraps a goroutine function with panic recovery, logging, and
// automatic restart with exponential backoff.  If the function panics or
// returns, it is restarted after an increasing delay (1s → 2s → 4s … capped
// at 30s).  The loop exits only when ctx is cancelled.
func (m *Manager) safeGo(ctx context.Context, name string, fn func()) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second

	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					m.logger.Error("background goroutine panicked, restarting",
						zap.String("goroutine", name),
						zap.Any("panic", r),
						zap.Duration("backoff", backoff),
					)
				}
			}()
			fn()
		}()

		// fn returned (or panicked) — restart unless context is done
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			m.logger.Info("restarting background goroutine",
				zap.String("goroutine", name),
			)
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}
