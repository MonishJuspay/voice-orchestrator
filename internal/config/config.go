// Package config handles application configuration from environment variables
// and tier configuration parsing.
//
// Tier configuration is stored in Redis (key: voice:tier:config) and cached
// in-memory with a sync.RWMutex for lock-free hot-path reads. A background
// goroutine in the pool manager refreshes the cache every 30s, so changes
// made to the Redis key take effect without a pod restart.
//
// On startup the env var TIER_CONFIG is parsed and written to Redis as the
// bootstrap/seed value. Subsequent changes should be made directly in Redis.
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// TierType represents the type of a pool tier.
type TierType string

const (
	TierTypeExclusive TierType = "exclusive"
	TierTypeShared    TierType = "shared"
)

// TierConfig represents configuration for a specific pool tier.
type TierConfig struct {
	Type          TierType `json:"type"`                     // "exclusive" or "shared"
	Target        int      `json:"target"`                   // Target number of pods in this tier
	MaxConcurrent int      `json:"max_concurrent,omitempty"` // Max concurrent calls per pod (shared tiers only)
}

// tierConfigEnvelope is the structured TIER_CONFIG JSON format.
// It supports both the new structured format and the legacy flat format.
//
// New format (preferred):
//
//	{
//	  "tiers": { "gold": {...}, "standard": {...}, "basic": {...} },
//	  "default_chain": ["gold", "standard", "basic"]
//	}
//
// Legacy flat format (still supported):
//
//	{ "gold": {"type":"exclusive","target":1}, "standard": {...} }
type tierConfigEnvelope struct {
	Tiers        map[string]TierConfig `json:"tiers"`
	DefaultChain []string              `json:"default_chain"`
}

// TierConfigRefreshInterval is how often the background goroutine refreshes
// the in-memory tier config from Redis.
const TierConfigRefreshInterval = 30 * time.Second

// Config holds all application configuration.
type Config struct {
	// Redis
	RedisURL         string
	RedisPoolSize    int
	RedisMinIdleConn int
	RedisMaxRetries  int
	RedisDialTimeout time.Duration

	// HTTP Server
	HTTPPort        string
	MetricsPort     string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration

	// Voice Agent
	VoiceAgentBaseURL string

	// Kubernetes
	Namespace        string
	PodLabelSelector string
	PodName          string

	// Pool Manager
	CleanupInterval   time.Duration
	LeaseTTL          time.Duration
	DrainingTTL       time.Duration
	ReconcileInterval time.Duration
	CallInfoTTL       time.Duration

	// Tier Configuration — guarded by tierMu.
	// NEVER access these directly; use the getter methods.
	TierConfigJSON   string // raw env var value (used for bootstrap only)
	parsedTierConfig map[string]TierConfig
	defaultChain     []string
	tierMu           sync.RWMutex

	// Leader Election
	LeaderElectionEnabled       bool
	LeaderElectionNamespace     string
	LeaderElectionLockName      string
	LeaderElectionDuration      time.Duration
	LeaderElectionRenewDeadline time.Duration
	LeaderElectionRetryPeriod   time.Duration

	// Logging
	LogLevel  string
	LogFormat string
}

// ---------------------------------------------------------------------------
// Thread-safe tier config getters
// ---------------------------------------------------------------------------

// GetParsedTierConfig returns a shallow copy of the tier config map.
// Safe for concurrent use — callers may iterate the returned map freely.
func (c *Config) GetParsedTierConfig() map[string]TierConfig {
	c.tierMu.RLock()
	defer c.tierMu.RUnlock()
	cp := make(map[string]TierConfig, len(c.parsedTierConfig))
	for k, v := range c.parsedTierConfig {
		cp[k] = v
	}
	return cp
}

// GetDefaultChain returns a copy of the default fallback chain.
func (c *Config) GetDefaultChain() []string {
	c.tierMu.RLock()
	defer c.tierMu.RUnlock()
	cp := make([]string, len(c.defaultChain))
	copy(cp, c.defaultChain)
	return cp
}

// IsSharedTier returns true if the given tier is configured as a shared pool.
func (c *Config) IsSharedTier(tier string) bool {
	c.tierMu.RLock()
	defer c.tierMu.RUnlock()
	cfg, exists := c.parsedTierConfig[tier]
	return exists && cfg.Type == TierTypeShared
}

// IsKnownTier returns true if the tier exists in the parsed tier config.
func (c *Config) IsKnownTier(tier string) bool {
	c.tierMu.RLock()
	defer c.tierMu.RUnlock()
	_, exists := c.parsedTierConfig[tier]
	return exists
}

// GetTierConfig returns the configuration for a specific tier.
func (c *Config) GetTierConfig(tier string) (TierConfig, bool) {
	c.tierMu.RLock()
	defer c.tierMu.RUnlock()
	cfg, exists := c.parsedTierConfig[tier]
	return cfg, exists
}

// TierNames returns all configured tier names (unordered).
func (c *Config) TierNames() []string {
	c.tierMu.RLock()
	defer c.tierMu.RUnlock()
	names := make([]string, 0, len(c.parsedTierConfig))
	for tier := range c.parsedTierConfig {
		names = append(names, tier)
	}
	return names
}

// ---------------------------------------------------------------------------
// Static tier helpers (no mutex needed — pure string operations)
// ---------------------------------------------------------------------------

// IsMerchantTier returns true if the tier string has the "merchant:" prefix.
func IsMerchantTier(tier string) bool {
	return strings.HasPrefix(tier, "merchant:")
}

// ParseMerchantTier extracts the merchant ID from a "merchant:xxx" tier string.
// Returns the merchant ID and true, or ("", false) if not a merchant tier.
func ParseMerchantTier(tier string) (merchantID string, ok bool) {
	if !strings.HasPrefix(tier, "merchant:") {
		return "", false
	}
	return tier[len("merchant:"):], true
}

// ---------------------------------------------------------------------------
// Loading & parsing
// ---------------------------------------------------------------------------

// Load loads configuration from environment variables.
func Load() (*Config, error) {
	cfg := &Config{
		// Redis defaults
		RedisURL:         getEnv("REDIS_URL", "redis://localhost:6379"),
		RedisPoolSize:    getEnvInt("REDIS_POOL_SIZE", 10),
		RedisMinIdleConn: getEnvInt("REDIS_MIN_IDLE_CONN", 5),
		RedisMaxRetries:  getEnvInt("REDIS_MAX_RETRIES", 3),
		RedisDialTimeout: getEnvDuration("REDIS_DIAL_TIMEOUT", 5*time.Second),

		// HTTP defaults
		HTTPPort:        getEnv("HTTP_PORT", "8080"),
		MetricsPort:     getEnv("METRICS_PORT", "9090"),
		ReadTimeout:     getEnvDuration("HTTP_READ_TIMEOUT", 5*time.Second),
		WriteTimeout:    getEnvDuration("HTTP_WRITE_TIMEOUT", 10*time.Second),
		IdleTimeout:     getEnvDuration("HTTP_IDLE_TIMEOUT", 60*time.Second),
		ShutdownTimeout: getEnvDuration("HTTP_SHUTDOWN_TIMEOUT", 30*time.Second),

		// Voice Agent
		VoiceAgentBaseURL: getEnv("VOICE_AGENT_BASE_URL", "wss://localhost:8081"),

		// Kubernetes
		Namespace:        getEnv("NAMESPACE", "default"),
		PodLabelSelector: getEnv("POD_LABEL_SELECTOR", "app=voice-agent"),
		PodName:          getEnv("POD_NAME", "smart-router-local"),

		// Pool Manager
		CleanupInterval:   getEnvDuration("CLEANUP_INTERVAL", 30*time.Second),
		LeaseTTL:          getEnvDuration("LEASE_TTL", 15*time.Minute),
		DrainingTTL:       getEnvDuration("DRAINING_TTL", 6*time.Minute),
		ReconcileInterval: getEnvDuration("RECONCILE_INTERVAL", 60*time.Second),
		CallInfoTTL:       getEnvDuration("CALL_INFO_TTL", 1*time.Hour),

		// Tier Config
		TierConfigJSON: getEnv("TIER_CONFIG", ""),

		// Leader Election
		LeaderElectionEnabled:       getEnvBool("LEADER_ELECTION_ENABLED", true),
		LeaderElectionNamespace:     getEnv("LEADER_ELECTION_NAMESPACE", ""),
		LeaderElectionLockName:      getEnv("LEADER_ELECTION_LOCK_NAME", "smart-router-leader"),
		LeaderElectionDuration:      getEnvDuration("LEADER_ELECTION_DURATION", 15*time.Second),
		LeaderElectionRenewDeadline: getEnvDuration("LEADER_ELECTION_RENEW_DEADLINE", 10*time.Second),
		LeaderElectionRetryPeriod:   getEnvDuration("LEADER_ELECTION_RETRY_PERIOD", 2*time.Second),

		// Logging
		LogLevel:  getEnv("LOG_LEVEL", "info"),
		LogFormat: getEnv("LOG_FORMAT", "json"),
	}

	if cfg.LeaderElectionNamespace == "" {
		cfg.LeaderElectionNamespace = cfg.Namespace
	}

	if err := cfg.parseTierConfig(); err != nil {
		return nil, fmt.Errorf("failed to parse tier config: %w", err)
	}

	return cfg, nil
}

// parseTierConfig parses the TIER_CONFIG JSON string into the in-memory cache.
//
// Supports three formats:
//  1. Structured (preferred): {"tiers": {...}, "default_chain": [...]}
//  2. Legacy flat:            {"gold": {"type":"exclusive","target":1}, ...}
//  3. Simple int map:         {"gold": 10, "standard": 20}
//
// When default_chain is not provided, it is built from the tier names in a
// deterministic order: exclusive tiers sorted alphabetically, then shared tiers.
func (c *Config) parseTierConfig() error {
	return c.applyTierConfigJSON(c.TierConfigJSON)
}

// applyTierConfigJSON parses a raw JSON string and swaps the in-memory tier
// config under the write lock. Used both at startup (from env var) and by the
// refresh goroutine (from Redis).
func (c *Config) applyTierConfigJSON(raw string) error {
	parsed := make(map[string]TierConfig)
	var chain []string

	if raw == "" {
		parsed = map[string]TierConfig{
			"gold":     {Type: TierTypeExclusive, Target: 10},
			"standard": {Type: TierTypeExclusive, Target: 20},
			"overflow": {Type: TierTypeExclusive, Target: 5},
		}
		chain = []string{"gold", "standard", "overflow"}
	} else {
		// 1. Try the new structured envelope
		var envelope tierConfigEnvelope
		if err := json.Unmarshal([]byte(raw), &envelope); err == nil && len(envelope.Tiers) > 0 {
			parsed = envelope.Tiers
			chain = envelope.DefaultChain
		} else {
			// 2. Try legacy flat map of TierConfig objects
			var flatConfig map[string]TierConfig
			if err := json.Unmarshal([]byte(raw), &flatConfig); err == nil && len(flatConfig) > 0 {
				hasStructured := false
				for _, tc := range flatConfig {
					if tc.Target > 0 || tc.Type != "" {
						hasStructured = true
						break
					}
				}
				if hasStructured {
					parsed = flatConfig
				}
			}

			// 3. Try simple map of tier name -> target count
			if len(parsed) == 0 {
				var simpleConfig map[string]int
				if err := json.Unmarshal([]byte(raw), &simpleConfig); err == nil && len(simpleConfig) > 0 {
					for tier, target := range simpleConfig {
						parsed[tier] = TierConfig{
							Type:   TierTypeExclusive,
							Target: target,
						}
					}
				}
			}

			if len(parsed) == 0 {
				return fmt.Errorf("invalid tier config JSON: cannot parse %q", raw)
			}
		}
	}

	// Normalize + ensure chain
	normalizeTierConfigs(parsed)
	chain = ensureDefaultChain(parsed, chain)

	// Swap under write lock
	c.tierMu.Lock()
	c.parsedTierConfig = parsed
	c.defaultChain = chain
	c.tierMu.Unlock()

	return nil
}

// ---------------------------------------------------------------------------
// Redis persistence: bootstrap + refresh
// ---------------------------------------------------------------------------

// TierConfigRedisKey is the Redis key where the canonical tier config lives.
const TierConfigRedisKey = "voice:tier:config"

// BootstrapTierConfigToRedis writes the current in-memory tier config to Redis
// using SETNX (SET if Not eXists). This seeds Redis on first deploy; after that,
// Redis is the source of truth and this call is a no-op.
func (c *Config) BootstrapTierConfigToRedis(ctx context.Context, client *redis.Client, logger *zap.Logger) {
	data := c.marshalTierConfig()
	set, err := client.SetNX(ctx, TierConfigRedisKey, data, 0).Result()
	if err != nil {
		logger.Warn("Failed to bootstrap tier config to Redis, will use env var",
			zap.Error(err))
		return
	}
	if set {
		logger.Info("Tier config bootstrapped to Redis (first deploy or key was missing)",
			zap.String("key", TierConfigRedisKey))
	} else {
		logger.Info("Tier config already exists in Redis, loading from Redis",
			zap.String("key", TierConfigRedisKey))
		// Redis already has a config — load it so we use the Redis version
		c.RefreshTierConfigFromRedis(ctx, client, logger)
	}
}

// RefreshTierConfigFromRedis reads the tier config from Redis and swaps the
// in-memory cache. If Redis is unavailable or the key is missing, the current
// in-memory config is kept unchanged.
func (c *Config) RefreshTierConfigFromRedis(ctx context.Context, client *redis.Client, logger *zap.Logger) {
	raw, err := client.Get(ctx, TierConfigRedisKey).Result()
	if err != nil {
		if err == redis.Nil {
			logger.Debug("Tier config key not found in Redis, keeping current config")
		} else {
			logger.Warn("Failed to read tier config from Redis, keeping current config",
				zap.Error(err))
		}
		return
	}

	if err := c.applyTierConfigJSON(raw); err != nil {
		logger.Error("Failed to parse tier config from Redis, keeping current config",
			zap.Error(err),
			zap.String("raw", raw))
		return
	}

	logger.Debug("Tier config refreshed from Redis")
}

// marshalTierConfig serialises the current in-memory tier config to the
// structured envelope JSON format for storage in Redis.
func (c *Config) marshalTierConfig() string {
	c.tierMu.RLock()
	defer c.tierMu.RUnlock()
	env := tierConfigEnvelope{
		Tiers:        c.parsedTierConfig,
		DefaultChain: c.defaultChain,
	}
	data, _ := json.Marshal(env)
	return string(data)
}

// ---------------------------------------------------------------------------
// Internal helpers (operate on values, not on Config — no mutex needed)
// ---------------------------------------------------------------------------

// normalizeTierConfigs fills in defaults for incomplete tier configs.
func normalizeTierConfigs(tiers map[string]TierConfig) {
	for tier, cfg := range tiers {
		if cfg.Type == "" {
			cfg.Type = TierTypeExclusive
		}
		if cfg.Type == TierTypeShared && cfg.MaxConcurrent == 0 {
			cfg.MaxConcurrent = 5
		}
		tiers[tier] = cfg
	}
}

// ensureDefaultChain builds a DefaultChain from tier names if one wasn't
// explicitly provided. Order: exclusive tiers sorted, then shared tiers sorted.
func ensureDefaultChain(tiers map[string]TierConfig, chain []string) []string {
	if len(chain) > 0 {
		// Validate every entry is a known tier
		valid := make([]string, 0, len(chain))
		for _, tier := range chain {
			if _, ok := tiers[tier]; ok {
				valid = append(valid, tier)
			}
		}
		if len(valid) > 0 {
			return valid
		}
	}

	// Build from tier names: exclusive first, then shared (sorted for determinism)
	exclusive := make([]string, 0)
	shared := make([]string, 0)
	for tier, cfg := range tiers {
		if cfg.Type == TierTypeShared {
			shared = append(shared, tier)
		} else {
			exclusive = append(exclusive, tier)
		}
	}
	sortStrings(exclusive)
	sortStrings(shared)
	return append(exclusive, shared...)
}

// sortStrings sorts a string slice in place (insertion sort for tiny slices).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// --- Environment variable helpers ---

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	result, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return result
}

func getEnvBool(key string, defaultValue bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	result, err := strconv.ParseBool(value)
	if err != nil {
		return defaultValue
	}
	return result
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	result, err := time.ParseDuration(value)
	if err != nil {
		return defaultValue
	}
	return result
}
