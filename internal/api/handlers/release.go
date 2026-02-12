package handlers

import (
	"encoding/json"
	"net/http"

	"go.uber.org/zap"

	"orchestration-api-go/internal/models"
	"orchestration-api-go/internal/releaser"
)

// ReleaseHandler handles pod release requests
type ReleaseHandler struct {
	releaser releaser.Interface
	logger   *zap.Logger
}

// NewReleaseHandler creates a new release handler
func NewReleaseHandler(releaser releaser.Interface, logger *zap.Logger) *ReleaseHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ReleaseHandler{
		releaser: releaser,
		logger:   logger,
	}
}

// Handle handles POST /api/v1/release
func (h *ReleaseHandler) Handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Decode JSON body
	var req models.ReleaseRequest
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.logger.Warn("failed to decode release request", zap.Error(err))
		respondWithError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate required fields
	if req.CallSID == "" {
		respondWithError(w, http.StatusBadRequest, "call_sid is required")
		return
	}

	// Call releaser
	result, err := h.releaser.Release(ctx, req.CallSID)
	if err != nil {
		h.logger.Error("release failed",
			zap.Error(err),
			zap.String("call_sid", req.CallSID),
		)

		switch err {
		case releaser.ErrCallNotFound:
			respondWithError(w, http.StatusNotFound, "call not found")
		default:
			respondWithError(w, http.StatusInternalServerError, "release failed")
		}
		return
	}

	// Return JSON response
	respondWithJSON(w, http.StatusOK, result)
}
