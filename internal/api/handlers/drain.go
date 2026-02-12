package handlers

import (
	"context"
	"encoding/json"
	"net/http"

	"go.uber.org/zap"

	"orchestration-api-go/internal/api/middleware"
	"orchestration-api-go/internal/models"
)

// Drainer defines the interface for draining pods
type Drainer interface {
	Drain(ctx context.Context, podName string) (*models.DrainResult, error)
}

// DrainHandler handles pod drain requests
type DrainHandler struct {
	drainer Drainer
	logger  *zap.Logger
}

// NewDrainHandler creates a new drain handler
func NewDrainHandler(drainer Drainer, logger *zap.Logger) *DrainHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &DrainHandler{
		drainer: drainer,
		logger:  logger,
	}
}

// Handle handles POST /api/v1/drain
func (h *DrainHandler) Handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Decode JSON body
	var req models.DrainRequest
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.logger.Warn("failed to decode drain request", zap.Error(err))
		respondWithError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate required fields
	if req.PodName == "" {
		respondWithError(w, http.StatusBadRequest, "pod_name is required")
		return
	}

	h.logger.Info("drain request received",
		zap.String("pod_name", req.PodName),
	)

	// Call drainer
	result, err := h.drainer.Drain(ctx, req.PodName)
	if err != nil {
		h.logger.Error("drain failed",
			zap.Error(err),
			zap.String("pod_name", req.PodName),
		)
		respondWithError(w, http.StatusInternalServerError, "drain failed")
		return
	}

	// Return JSON response
	middleware.DrainsTotal.Inc()
	respondWithJSON(w, http.StatusOK, result)
}
