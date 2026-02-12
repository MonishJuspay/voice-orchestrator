-- Atomic allocation from shared pool (ZSET)
-- KEYS[1] = pool key (ZSET)
-- ARGV[1] = max_concurrent (capacity limit)
-- ARGV[2] = draining key prefix
-- Returns: pod_name on success, nil on failure

-- Step 1: Get the pod with least connections from the pool
-- ZRANGE key 0 0 WITHSCORES returns the member with lowest score
local result = redis.call('ZRANGE', KEYS[1], 0, 0, 'WITHSCORES')

-- Step 2: Check if any pods exist in the pool
if #result == 0 then
    -- No pods available in the pool
    return nil
end

local pod_name = result[1]
local score = tonumber(result[2])

-- Step 3: Check if the pool is at capacity (score >= max_concurrent)
local max_concurrent = tonumber(ARGV[1])
if score >= max_concurrent then
    -- Pool is at capacity, cannot allocate more connections
    return nil
end

-- Step 4: Check if the pod is draining
-- Construct the draining key: draining_key_prefix + pod_name
local draining_key = ARGV[2] .. pod_name
local is_draining = redis.call('EXISTS', draining_key)

if is_draining == 1 then
    -- Pod is draining, remove it from the pool and return nil
    redis.call('ZREM', KEYS[1], pod_name)
    return nil
end

-- Step 5: Increment the connection count for this pod
-- ZINCRBY increments the score by 1
redis.call('ZINCRBY', KEYS[1], 1, pod_name)

-- Step 6: Return the pod name for connection allocation
return pod_name
