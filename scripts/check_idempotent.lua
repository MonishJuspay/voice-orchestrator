-- Check if call_sid already has allocation
-- KEYS[1] = call info key (HASH)
-- Returns: pod_name if call has allocation, nil otherwise

-- Step 1: Get the pod_name field from the call info hash
-- This is used for idempotency - checking if a call already has a pod assigned
local pod_name = redis.call('HGET', KEYS[1], 'pod_name')

-- Step 2: Return the result
-- Returns the pod_name if found, or nil if the call has no allocation
return pod_name
