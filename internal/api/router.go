package api

import (
	"context"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"orchestration-api-go/internal/allocator"
	"orchestration-api-go/internal/api/handlers"
	"orchestration-api-go/internal/api/middleware"
	"orchestration-api-go/internal/config"
	"orchestration-api-go/internal/models"
	"orchestration-api-go/internal/redisclient"
	"orchestration-api-go/internal/releaser"
)

// Drainer defines the interface for draining pods
type Drainer interface {
	Drain(ctx context.Context, podName string) (*models.DrainResult, error)
}

// LeaderChecker provides leader election status
type LeaderChecker interface {
	IsLeader() bool
}

// NewRouter creates a new Chi router with all routes and middleware configured
func NewRouter(
	alloc allocator.Interface,
	rel releaser.Interface,
	drainer Drainer,
	redis *redisclient.Client,
	cfg *config.Config,
	logger *zap.Logger,
	leader LeaderChecker,
) chi.Router {
	r := chi.NewRouter()

	// Apply middleware stack
	r.Use(middleware.Recovery(logger))
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(middleware.Logger(logger))
	r.Use(middleware.Metrics)
	r.Use(chimiddleware.Timeout(60 * time.Second))

	// Initialize handlers
	allocateHandler := handlers.NewAllocateHandler(alloc, logger)
	releaseHandler := handlers.NewReleaseHandler(rel, logger)
	statusHandler := handlers.NewStatusHandler(redis, cfg, logger, leader)
	drainHandler := handlers.NewDrainHandler(drainer, logger)
	healthHandler := handlers.NewHealthHandler(redis, logger)
	podInfoHandler := handlers.NewPodInfoHandler(redis, logger)

	// API v1 routes
	r.Route("/api/v1", func(r chi.Router) {
		// Allocation endpoints
		r.Post("/allocate", allocateHandler.Handle)
		r.Post("/twilio/allocate", allocateHandler.HandleTwilio)
		r.Post("/plivo/allocate", allocateHandler.HandlePlivo)
		r.Post("/exotel/allocate", allocateHandler.HandleExotel)

		// Release endpoint
		r.Post("/release", releaseHandler.Handle)

		// Status endpoint
		r.Get("/status", statusHandler.Handle)

		// Drain endpoint
		r.Post("/drain", drainHandler.Handle)

		// Pod Info endpoint
		r.Get("/pod/{pod_name}", podInfoHandler.Handle)

		// Health and readiness endpoints
		r.Get("/health", healthHandler.HandleHealth)
		r.Get("/ready", healthHandler.HandleReady)
	
		// Metrics endpoint
		r.Get("/metrics", promhttp.Handler().ServeHTTP)
	})


	return r
}
