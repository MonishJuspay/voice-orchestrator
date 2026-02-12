package handlers

import (
	"net/http"

	"go.uber.org/zap"

	"orchestration-api-go/internal/models"
	"orchestration-api-go/internal/redisclient"
)

// HealthHandler handles health and readiness checks
type HealthHandler struct {
	redis  *redisclient.Client
	logger *zap.Logger
}

// NewHealthHandler creates a new health handler
func NewHealthHandler(redis *redisclient.Client, logger *zap.Logger) *HealthHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &HealthHandler{
		redis:  redis,
		logger: logger,
	}
}

// HandleHealth handles GET /api/v1/health (liveness probe)
// Returns 200 unconditionally — the process is alive.
// K8s liveness should NOT depend on external services (Redis),
// otherwise a Redis outage cascades into pod restarts.
func (h *HealthHandler) HandleHealth(w http.ResponseWriter, r *http.Request) {
	response := models.HealthResponse{
		Status: "ok",
	}
	respondWithJSON(w, http.StatusOK, response)
}

// HandleReady handles GET /api/v1/ready (readiness probe)
// Checks Redis connectivity — only mark ready if we can serve traffic.
func (h *HealthHandler) HandleReady(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if err := h.redis.Ping(ctx); err != nil {
		h.logger.Error("readiness check failed: redis unavailable", zap.Error(err))
		respondWithError(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}

	response := map[string]string{
		"status": "ready",
	}
	respondWithJSON(w, http.StatusOK, response)
}
