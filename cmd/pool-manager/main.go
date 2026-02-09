package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/MonishJuspay/voice-orchestrator/internal/app/poolmanager"
	"github.com/MonishJuspay/voice-orchestrator/internal/config"
	"github.com/MonishJuspay/voice-orchestrator/pkg/logger"
	"go.uber.org/zap"
)

func main() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize logger
	if err := logger.InitLogger(cfg.LogLevel, cfg.LogFormat); err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}
	defer logger.Sync()

	logger.Info("Starting Voice Orchestrator Pool Manager",
		zap.String("version", cfg.AppVersion),
		zap.String("log_level", cfg.LogLevel),
		zap.Int("reconcile_interval_seconds", cfg.ReconcileIntervalSeconds),
	)

	// Create pool manager
	pm, err := poolmanager.New(cfg)
	if err != nil {
		logger.Fatal("Failed to create pool manager", zap.Error(err))
	}

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		logger.Info("Shutdown signal received", zap.String("signal", sig.String()))
		cancel()
	}()

	// Start pool manager
	logger.Info("Pool manager starting reconciliation loop")

	if err := pm.Start(ctx); err != nil {
		logger.Fatal("Pool manager failed", zap.Error(err))
	}

	logger.Info("Pool manager shutdown complete")
}
