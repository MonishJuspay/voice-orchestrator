package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/MonishJuspay/voice-orchestrator/internal/app/router"
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

	logger.Info("Starting Voice Orchestrator Router",
		zap.String("version", cfg.AppVersion),
		zap.String("log_level", cfg.LogLevel),
	)

	// Create router server
	srv, err := router.NewServer(cfg)
	if err != nil {
		logger.Fatal("Failed to create server", zap.Error(err))
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

	// Start server
	logger.Info("Router is ready to accept requests",
		zap.String("address", cfg.GetServerAddress()),
	)

	if err := srv.Start(ctx); err != nil {
		logger.Fatal("Server failed", zap.Error(err))
	}

	logger.Info("Router shutdown complete")
}
