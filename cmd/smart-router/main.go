package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"orchestration-api-go/internal/allocator"
	"orchestration-api-go/internal/api"
	"orchestration-api-go/internal/config"
	"orchestration-api-go/internal/poolmanager"
	"orchestration-api-go/internal/redisclient"
	"orchestration-api-go/internal/releaser"
	"orchestration-api-go/internal/drainer"
)

func main() {
	// Create root context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// Setup logger
	logger, err := setupLogger(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to setup logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	logger.Info("Starting Smart Router",
		zap.String("version", "1.0.0"),
		zap.String("pod_name", cfg.PodName),
	)

	// Create Redis client
	redisClient, err := redisclient.NewClient(cfg)
	if err != nil {
		logger.Fatal("Failed to create Redis client", zap.Error(err))
	}
	defer func() {
		if err := redisClient.Close(); err != nil {
			logger.Error("Error closing Redis connection", zap.Error(err))
		}
	}()

	// Test Redis connection
	if err := redisClient.Ping(ctx); err != nil {
		logger.Fatal("Failed to connect to Redis", zap.Error(err))
	}
	logger.Info("Connected to Redis")

	// Bootstrap tier config to Redis (SETNX — only seeds on first deploy).
	// If Redis already has a config, this loads it into memory so we use the
	// Redis version instead of the env var.
	cfg.BootstrapTierConfigToRedis(ctx, redisClient.GetRedis(), logger)

	// Start background tier config refresh (runs on ALL replicas, not just leader)
	go runTierConfigRefresh(ctx, cfg, redisClient, logger)


	// Create Kubernetes client (if in cluster)
	var k8sClient *kubernetes.Clientset
	if cfg.LeaderElectionEnabled {
		k8sClient, err = createK8sClient()
		if err != nil {
			logger.Warn("Failed to create Kubernetes client, disabling leader election",
				zap.Error(err))
			cfg.LeaderElectionEnabled = false
		} else {
			logger.Info("Kubernetes client created successfully")
		}
	}

	// Create components
	alloc := allocator.NewAllocator(redisClient.GetRedis(), cfg, logger)
	rel := releaser.NewReleaser(redisClient, cfg, logger)

	// Create drainer
	podDrainer := drainer.NewDrainer(redisClient, cfg, logger)

	// Create pool manager (before router so we can wire leader status)
	var poolManager *poolmanager.Manager
	if cfg.LeaderElectionEnabled && k8sClient != nil {
		poolManager = poolmanager.NewManager(k8sClient, redisClient.GetRedis(), cfg, logger)
	}

	// Create router (with leader checker — nil-safe, returns false when no pool manager)
	var leader api.LeaderChecker
	if poolManager != nil {
		leader = poolManager
	}
	router := api.NewRouter(alloc, rel, podDrainer, redisClient, cfg, logger, leader)

	// Start HTTP server
	httpServer := &http.Server{
		Addr:         ":" + cfg.HTTPPort,
		Handler:      router,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	// Start metrics server (if different port) — separate minimal mux for security
	var metricsServer *http.Server
	if cfg.MetricsPort != cfg.HTTPPort {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.Handler())
		metricsMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
		})
		metricsServer = &http.Server{
			Addr:    ":" + cfg.MetricsPort,
			Handler: metricsMux,
		}
	}

	// Start health check goroutine
	go runHealthChecks(ctx, redisClient, logger)

	// Start pool manager if created
	if poolManager != nil {
		go func() {
			logger.Info("Starting pool manager with leader election")
			if err := poolManager.Run(ctx); err != nil {
				logger.Error("Pool manager stopped with error", zap.Error(err))
			}
		}()
	}

	// Setup graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Start servers in goroutines
	go func() {
		logger.Info("Starting HTTP server", zap.String("port", cfg.HTTPPort))
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("HTTP server failed", zap.Error(err))
		}
	}()

	if metricsServer != nil {
		go func() {
			logger.Info("Starting metrics server", zap.String("port", cfg.MetricsPort))
			if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("Metrics server failed", zap.Error(err))
			}
		}()
	}

	logger.Info("Smart Router started successfully",
		zap.String("http_port", cfg.HTTPPort),
		zap.String("metrics_port", cfg.MetricsPort),
		zap.Bool("leader_election", cfg.LeaderElectionEnabled),
	)

	// Wait for shutdown signal
	<-quit
	logger.Info("Shutdown signal received, initiating graceful shutdown...")

	// Create shutdown context with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutdownCancel()

	// Cancel root context to stop background processes
	cancel()

	// Shutdown HTTP server
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", zap.Error(err))
	} else {
		logger.Info("HTTP server shut down gracefully")
	}

	// Shutdown metrics server if running
	if metricsServer != nil {
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("Metrics server shutdown error", zap.Error(err))
		}
	}

	logger.Info("Smart Router shutdown complete")
}

func setupLogger(cfg *config.Config) (*zap.Logger, error) {
	level := zapcore.InfoLevel
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = zapcore.InfoLevel
	}

	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(level)

	if cfg.LogFormat == "console" {
		config.Encoding = "console"
		config.EncoderConfig = zap.NewDevelopmentEncoderConfig()
	}

	return config.Build()
}

func createK8sClient() (*kubernetes.Clientset, error) {
	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	return clientset, nil
}

func runHealthChecks(ctx context.Context, redisClient *redisclient.Client, logger *zap.Logger) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := redisClient.Ping(ctx); err != nil {
				logger.Warn("Redis health check failed", zap.Error(err))
			}
		}
	}
}

// runTierConfigRefresh periodically reads the tier config from Redis and swaps
// the in-memory cache. Runs on ALL replicas so that config changes propagate
// without pod restarts.
func runTierConfigRefresh(ctx context.Context, cfg *config.Config, redisClient *redisclient.Client, logger *zap.Logger) {
	ticker := time.NewTicker(config.TierConfigRefreshInterval)
	defer ticker.Stop()

	logger.Info("Tier config refresh started",
		zap.Duration("interval", config.TierConfigRefreshInterval))

	for {
		select {
		case <-ctx.Done():
			logger.Info("Tier config refresh stopped")
			return
		case <-ticker.C:
			cfg.RefreshTierConfigFromRedis(ctx, redisClient.GetRedis(), logger)
		}
	}
}