package allocator

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/redis/go-redis/v9"
	"orchestration-api-go/internal/models"
)

const (
	merchantConfigHash = "voice:merchant:config"
)

// GetMerchantConfig retrieves the allocation configuration for a merchant from
// Redis. If the merchant is not found or the ID is empty, returns an empty
// MerchantConfig — the allocator will fall back to the system-wide DefaultChain.
func GetMerchantConfig(ctx context.Context, client *redis.Client, merchantID string) (models.MerchantConfig, error) {
	if merchantID == "" {
		return models.MerchantConfig{}, nil
	}

	configJSON, err := client.HGet(ctx, merchantConfigHash, merchantID).Result()
	if err == redis.Nil {
		return models.MerchantConfig{}, nil
	}
	if err != nil {
		return models.MerchantConfig{}, fmt.Errorf("redis hget failed: %w", err)
	}

	var mc models.MerchantConfig
	if err := json.Unmarshal([]byte(configJSON), &mc); err != nil {
		return models.MerchantConfig{}, fmt.Errorf("malformed merchant config JSON for %q: %w", merchantID, err)
	}

	return mc, nil
}
