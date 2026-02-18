-- release.lua — Atomic decrement for connection release.
--
-- KEYS[1] = proxy:bucket:{bucket_id}:count       (global connection count)
-- KEYS[2] = proxy:instance:{instance_id}:conns    (hash: bucket_id → local count)
--
-- ARGV[1] = bucket_id
-- ARGV[2] = channel name for Pub/Sub notification
--
-- Returns:
--   >=0 = new global count (release succeeded)
--   -1  = count was already 0 (underflow protection)

local count_key = KEYS[1]
local inst_key  = KEYS[2]
local bucket_id = ARGV[1]
local channel   = ARGV[2]

local current = tonumber(redis.call('GET', count_key) or 0)

if current <= 0 then
    -- Underflow protection: don't go below 0
    redis.call('SET', count_key, 0)
    return -1
end

local new_count = redis.call('DECR', count_key)

-- Decrement this instance's tracked count
local inst_count = tonumber(redis.call('HGET', inst_key, bucket_id) or 0)
if inst_count > 0 then
    redis.call('HINCRBY', inst_key, bucket_id, -1)
end

-- Notify waiting instances that a connection was freed
redis.call('PUBLISH', channel, bucket_id)

return new_count
