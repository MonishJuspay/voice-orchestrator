package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/MonishJuspay/voice-orchestrator/internal/config"
	"github.com/MonishJuspay/voice-orchestrator/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Server represents the HTTP server
type Server struct {
	config     *config.Config
	router     *gin.Engine
	httpServer *http.Server
	handler    *Handler
}

// NewServer creates a new HTTP server instance
func NewServer(cfg *config.Config) (*Server, error) {
	// Set Gin mode based on log level
	if cfg.LogLevel == "debug" {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	// Create router with default middleware
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(LoggingMiddleware())
	r.Use(CORSMiddleware())

	// Create handler
	handler := NewHandler(cfg)

	// Setup routes
	setupRoutes(r, handler)

	// Create HTTP server
	httpServer := &http.Server{
		Addr:         cfg.GetServerAddress(),
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return &Server{
		config:     cfg,
		router:     r,
		httpServer: httpServer,
		handler:    handler,
	}, nil
}

// Start starts the HTTP server
func (s *Server) Start(ctx context.Context) error {
	logger.Info("Starting HTTP server",
		zap.String("address", s.config.GetServerAddress()),
		zap.String("version", s.config.AppVersion),
	)

	// Start server in a goroutine
	errChan := make(chan error, 1)
	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- fmt.Errorf("server failed to start: %w", err)
		}
	}()

	// Wait for context cancellation or error
	select {
	case <-ctx.Done():
		logger.Info("Shutdown signal received, stopping server...")
		return s.Shutdown()
	case err := <-errChan:
		return err
	}
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	logger.Info("Shutting down HTTP server...")
	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("server shutdown failed: %w", err)
	}

	logger.Info("HTTP server stopped successfully")
	return nil
}
