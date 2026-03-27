package handlers

import (
	"net/http"

	"go.uber.org/zap"

	"orchestration-api-go/internal/config"
	"orchestration-api-go/internal/models"
	"orchestration-api-go/internal/redisclient"
)

// LeaderChecker provides leader election status
type LeaderChecker interface {
	IsLeader() bool
}

// StatusHandler handles status requests
type StatusHandler struct {
	redis  *redisclient.Client
	config *config.Config
	leader LeaderChecker
	logger *zap.Logger
}

// NewStatusHandler creates a new status handler
func NewStatusHandler(redis *redisclient.Client, cfg *config.Config, logger *zap.Logger, leader LeaderChecker) *StatusHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &StatusHandler{
		redis:  redis,
		config: cfg,
		leader: leader,
		logger: logger,
	}
}

// Handle handles GET /api/v1/status
func (h *StatusHandler) Handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get Redis PING status
	redisStatus := "up"
	if err := h.redis.Ping(ctx); err != nil {
		redisStatus = "down"
		h.logger.Error("status check: redis down", zap.Error(err))
	}

	// Build pool info from config + Redis, and compute active calls from pool
	// state. Previous approach counted voice:lease:* keys, which is unreliable
	// because leases have a 15-min TTL and persist after calls end (stale leases
	// inflate the count). Pool state (assigned - available for exclusive tiers,
	// sum of ZSET scores for shared tiers) is always authoritative.
	activeCalls := 0
	pools := make(map[string]models.PoolInfo)
	if redisStatus == "up" && h.config != nil {
		client := h.redis.GetRedis()

		// Build a set of tiers in the default chain so we can distinguish
		// merchant tiers (not in chain) from regular tiers.
		defaultChain := h.config.GetDefaultChain()
		chainSet := make(map[string]bool, len(defaultChain))
		for _, t := range defaultChain {
			chainSet[t] = true
		}

		for tier, cfg := range h.config.GetParsedTierConfig() {
			isMerchant := !chainSet[tier]

			var assignedKey, availableKey string
			if isMerchant {
				assignedKey = redisclient.MerchantAssignedKey(tier)
				availableKey = redisclient.MerchantPodsKey(tier)
			} else {
				assignedKey = redisclient.PoolAssignedKey(tier)
				availableKey = redisclient.PoolAvailableKey(tier)
			}

			assigned, err := client.SCard(ctx, assignedKey).Result()
			if err != nil {
				h.logger.Warn("failed to get assigned count", zap.String("tier", tier), zap.Error(err))
			}

			var available int64
			if !isMerchant && cfg.Type == config.TierTypeShared {
				available, err = client.ZCard(ctx, availableKey).Result()
				if err != nil {
					h.logger.Warn("failed to get available count", zap.String("tier", tier), zap.Error(err))
				}
				// For shared pools, active calls = sum of ZSET scores
				// (each score represents concurrent calls on that pod)
				scores, zErr := client.ZRangeWithScores(ctx, availableKey, 0, -1).Result()
				if zErr == nil {
					for _, z := range scores {
						if z.Score > 0 {
							activeCalls += int(z.Score)
						}
					}
				}
			} else {
				available, err = client.SCard(ctx, availableKey).Result()
				if err != nil {
					h.logger.Warn("failed to get available count", zap.String("tier", tier), zap.Error(err))
				}
				// For exclusive pools, active calls = assigned - available
				busy := int(assigned) - int(available)
				if busy > 0 {
					activeCalls += busy
				}
			}

			pools[tier] = models.PoolInfo{
				Type:      string(cfg.Type),
				Assigned:  int(assigned),
				Available: int(available),
			}
		}
	}

	// Determine leader status
	isLeader := false
	if h.leader != nil {
		isLeader = h.leader.IsLeader()
	}

	response := models.StatusResponse{
		Pools:       pools,
		ActiveCalls: activeCalls,
		IsLeader:    isLeader,
		Status:      redisStatus,
	}

	h.logger.Debug("status request served",
		zap.Int("active_calls", activeCalls),
		zap.Bool("is_leader", isLeader),
		zap.Int("pool_count", len(pools)),
	)

	respondWithJSON(w, http.StatusOK, response)
}
