package releaser

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Lua script for atomic ZINCRBY -1 with floor at 0
// KEYS[1] = pool key (ZSET)
// ARGV[1] = pod name
// Returns: new score (as integer), or -1 if pod not in set
const releaseSharedScript = `
local pool_key = KEYS[1]
local pod_name = ARGV[1]

-- Get current score
local current_score = redis.call('ZSCORE', pool_key, pod_name)
if current_score == false then
    return -1
end

local score = tonumber(current_score)

-- Only decrement if score > 0
if score > 0 then
    local new_score = redis.call('ZINCRBY', pool_key, -1, pod_name)
    return tonumber(new_score)
else
    return 0
end
`

// releaseToSharedPool decrements the connection count for a pod in a shared pool (ZSET)
// It uses a Lua script for atomic ZINCRBY -1 with floor at 0
// Returns the new score (number of active connections)
func releaseToSharedPool(ctx context.Context, redis *redis.Client, poolKey, podName string) (int64, error) {
	// Run the Lua script atomically
	result, err := redis.Eval(ctx, releaseSharedScript, []string{poolKey}, podName).Result()
	if err != nil {
		return 0, fmt.Errorf("lua script failed: %w", err)
	}

	// Handle result
	switch v := result.(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	default:
		return 0, fmt.Errorf("unexpected result type from lua script: %T", result)
	}
}
