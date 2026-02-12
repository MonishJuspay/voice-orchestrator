package allocator

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"orchestration-api-go/internal/models"
)

// Lua script for atomic idempotency check + lock acquisition.
// Returns:
//   - Existing call data (array of field/value pairs) if call already allocated
//   - Empty array if lock acquired (caller should proceed with allocation)
//
// The lock is a minimal hash with 30s TTL. If the allocator crashes after
// acquiring the lock, the TTL ensures cleanup.
const checkAndLockScript = `
local call_key = KEYS[1]

-- Check if call already has allocation data
local existing = redis.call('HGETALL', call_key)
if #existing > 0 then
    return existing
end

-- Atomically claim this call_sid with a placeholder lock
redis.call('HSET', call_key, '_lock', '1')
redis.call('EXPIRE', call_key, 30)
return {}
`

// CheckAndLockAllocation atomically checks for an existing allocation and
// acquires a lock if none exists. Returns:
//   - (result, nil) if an existing allocation was found
//   - (nil, nil) if the lock was acquired (caller should proceed)
//   - (nil, err) on Redis error
func CheckAndLockAllocation(ctx context.Context, client *redis.Client, callSID string) (*models.AllocationResult, error) {
	callKey := callInfoPrefix + callSID

	result, err := client.Eval(ctx, checkAndLockScript, []string{callKey}).Result()
	if err != nil {
		return nil, fmt.Errorf("idempotency check failed: %w", err)
	}

	// Parse result — empty array means lock acquired
	items, ok := result.([]interface{})
	if !ok || len(items) == 0 {
		return nil, nil // Lock acquired, proceed with allocation
	}

	// Parse existing allocation from field/value pairs
	callInfo := make(map[string]string)
	for i := 0; i+1 < len(items); i += 2 {
		key, _ := items[i].(string)
		val, _ := items[i+1].(string)
		if key != "" && key != "_lock" {
			callInfo[key] = val
		}
	}

	// If we only got the lock placeholder back (race between lock set and
	// real data write), treat as "not yet allocated" — return nil so caller
	// retries or waits briefly.
	if callInfo["pod_name"] == "" {
		return nil, nil
	}

	allocation := &models.AllocationResult{
		PodName:     callInfo["pod_name"],
		SourcePool:  callInfo["source_pool"],
		WasExisting: true,
	}

	if allocatedAtStr := callInfo["allocated_at"]; allocatedAtStr != "" {
		if timestamp, err := strconv.ParseInt(allocatedAtStr, 10, 64); err == nil {
			allocation.AllocatedAt = time.Unix(timestamp, 0)
		}
	}

	return allocation, nil
}
