-- Atomic release to shared pool (ZSET)
-- KEYS[1] = pool key (ZSET)
-- ARGV[1] = pod_name
-- Returns: new score on success, 0 if pod not found or already at 0

-- Step 1: Get current connection count for the pod
local current_score = redis.call('ZSCORE', KEYS[1], ARGV[1])

-- Step 2: Check if pod exists in the pool and has connections
if current_score == false then
    -- Pod not found in the pool
    return 0
end

local score = tonumber(current_score)
if score <= 0 then
    -- Pod has no active connections to release
    return 0
end

-- Step 3: Decrement the connection count by 1
-- ZINCRBY with -1 decrements the score
local new_score = redis.call('ZINCRBY', KEYS[1], -1, ARGV[1])

-- Step 4: Return the new connection count
return tonumber(new_score)
