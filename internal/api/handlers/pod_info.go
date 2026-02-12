package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"orchestration-api-go/internal/redisclient"
)

// PodInfoHandler handles pod information requests
type PodInfoHandler struct {
	redis  *redisclient.Client
	logger *zap.Logger
}

// NewPodInfoHandler creates a new pod info handler
func NewPodInfoHandler(redis *redisclient.Client, logger *zap.Logger) *PodInfoHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &PodInfoHandler{
		redis:  redis,
		logger: logger,
	}
}

// PodInfoResponse represents the response for pod info
type PodInfoResponse struct {
	PodName        string `json:"pod_name"`
	Tier           string `json:"tier"`
	IsDraining     bool   `json:"is_draining"`
	HasActiveLease bool   `json:"has_active_lease"`
	LeaseCallSID   string `json:"lease_call_sid,omitempty"`
}

// Handle handles GET /api/v1/pod/{pod_name}
func (h *PodInfoHandler) Handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	podName := chi.URLParam(r, "pod_name")

	if podName == "" {
		respondWithError(w, http.StatusBadRequest, "pod_name is required")
		return
	}

	client := h.redis.GetRedis()

	// 1. Get Pod Tier
	tierKey := redisclient.PodTierKey(podName)
	tier, err := client.Get(ctx, tierKey).Result()
	if err == redis.Nil {
		respondWithError(w, http.StatusNotFound, "pod not found")
		return
	}
	if err != nil {
		h.logger.Error("failed to get pod tier", zap.Error(err))
		respondWithError(w, http.StatusInternalServerError, "failed to get pod info")
		return
	}

	// 2. Check Draining Status
	drainingKey := redisclient.DrainingKey(podName)
	isDraining, err := client.Exists(ctx, drainingKey).Result()
	if err != nil {
		h.logger.Error("failed to check draining status", zap.Error(err))
	}

	// 3. Check Active Lease
	leaseKey := redisclient.LeaseKey(podName)
	leaseCallSID, err := client.Get(ctx, leaseKey).Result()
	hasActiveLease := err == nil && leaseCallSID != ""

	response := PodInfoResponse{
		PodName:        podName,
		Tier:           tier,
		IsDraining:     isDraining > 0,
		HasActiveLease: hasActiveLease,
		LeaseCallSID:   leaseCallSID,
	}

	respondWithJSON(w, http.StatusOK, response)
}
