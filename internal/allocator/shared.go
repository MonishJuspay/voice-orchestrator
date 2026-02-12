package allocator

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Lua script for atomic check-and-increment on sorted set
// KEYS[1] = pool key (ZSET)
// ARGV[1] = max concurrent connections allowed
// Returns: pod name if successful, nil if no capacity
//
// Iterates through all ZSET members (sorted by score ascending) to find the
// first non-draining pod with capacity. This avoids returning nil when the
// lowest-score pod happens to be draining but other pods are available.
const allocateSharedScript = `
local pool_key = KEYS[1]
local max_concurrent = tonumber(ARGV[1])

-- Get all pods sorted by score ascending (least connections first)
local result = redis.call('ZRANGE', pool_key, 0, -1, 'WITHSCORES')
if #result == 0 then
    return nil
end

-- Iterate pairs: result[i]=name, result[i+1]=score
for i = 1, #result, 2 do
    local pod_name = result[i]
    local current_score = tonumber(result[i + 1])

    -- Check if pod has capacity
    if current_score >= max_concurrent then
        -- All subsequent pods will have equal or higher scores, so stop
        return nil
    end

    -- Check if pod is draining
    local draining_key = "voice:pod:draining:" .. pod_name
    if redis.call('EXISTS', draining_key) == 0 then
        -- Not draining, has capacity — allocate it
        redis.call('ZINCRBY', pool_key, 1, pod_name)
        return pod_name
    end
    -- Pod is draining, try next one
end

-- All pods are either draining or at capacity
return nil
`

// tryAllocateShared attempts to allocate a pod from a shared pool using ZSET
// It uses a Lua script for atomic check-and-increment
func tryAllocateShared(ctx context.Context, client *redis.Client, tier string, maxConcurrent int) (string, error) {
	if maxConcurrent <= 0 {
		maxConcurrent = 5 // Default
	}

	poolKey := "voice:pool:" + tier + ":available"

	// Run the Lua script atomically.
	// The Lua script returns nil when no pods have capacity — go-redis surfaces
	// that as err == redis.Nil.  We must distinguish this from real errors
	// (connection failures, script syntax errors, etc.).
	result, err := client.Eval(ctx, allocateSharedScript, []string{poolKey}, maxConcurrent).Result()
	if err == redis.Nil || result == nil {
		return "", ErrNoPodsAvailable
	}
	if err != nil {
		return "", fmt.Errorf("lua script failed: %w", err)
	}

	podName, ok := result.(string)
	if !ok {
		return "", fmt.Errorf("unexpected result type from lua script")
	}

	return podName, nil
}
