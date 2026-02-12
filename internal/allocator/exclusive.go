package allocator

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// tryAllocateExclusive attempts to allocate a pod from an exclusive pool using SPOP
// It atomically removes and returns a pod from the set, then checks draining status
func tryAllocateExclusive(ctx context.Context, client *redis.Client, poolKey, drainingPrefix string) (string, error) {
	const maxAttempts = 10

	for i := 0; i < maxAttempts; i++ {
		// SPOP: Atomically remove and return a random member from the set
		podName, err := client.SPop(ctx, poolKey).Result()
		if err == redis.Nil {
			// Pool is empty
			return "", ErrNoPodsAvailable
		}
		if err != nil {
			return "", fmt.Errorf("redis spop failed: %w", err)
		}

		// Check if pod is draining
		drainingKey := drainingPrefix + podName
		exists, err := client.Exists(ctx, drainingKey).Result()
		if err != nil {
			// Error checking draining status, return pod to pool
			client.SAdd(ctx, poolKey, podName)
			return "", fmt.Errorf("failed to check draining status: %w", err)
		}

		if exists > 0 {
			// Pod is draining, skip it (don't return to pool)
			continue
		}

		// Pod is available and not draining
		return podName, nil
	}

	// Exceeded max attempts, likely all pods are draining
	return "", ErrDrainingPod
}
