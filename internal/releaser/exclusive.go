package releaser

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"orchestration-api-go/internal/redisclient"
)

// releaseToExclusivePool returns a pod to an exclusive pool (SET)
// It uses SAdd to add the pod back to the pool only if the pod is not draining
func releaseToExclusivePool(ctx context.Context, redis *redis.Client, poolKey, podName string) error {
	// Check if pod is draining before adding back
	drainingKey := redisclient.DrainingKey(podName)
	isDraining, err := redis.Exists(ctx, drainingKey).Result()
	if err != nil {
		return fmt.Errorf("failed to check draining status: %w", err)
	}

	// If pod is draining, don't add back to pool
	if isDraining > 0 {
		return nil
	}

	// Add pod back to exclusive pool
	if err := redis.SAdd(ctx, poolKey, podName).Err(); err != nil {
		return fmt.Errorf("failed to add pod to exclusive pool: %w", err)
	}

	return nil
}
