-- acquire.lua — Atomic check-and-increment for connection acquire.
--
-- KEYS[1] = proxy:bucket:{bucket_id}:count       (global connection count)
-- KEYS[2] = proxy:bucket:{bucket_id}:max          (max connections allowed)
-- KEYS[3] = proxy:instance:{instance_id}:conns    (hash: bucket_id → local count)
--
-- ARGV[1] = bucket_id
-- ARGV[2] = instance_id
--
-- Returns:
--   >0  = new global count (acquire succeeded)
--   -1  = pool is at max capacity (acquire rejected)
--   -2  = error (should not happen)

local count_key = KEYS[1]
local max_key   = KEYS[2]
local inst_key  = KEYS[3]
local bucket_id = ARGV[1]

local current = tonumber(redis.call('GET', count_key) or 0)
local max     = tonumber(redis.call('GET', max_key) or 0)

if max == 0 then
    -- max not set yet — should not happen, but be safe
    return -2
end

if current < max then
    local new_count = redis.call('INCR', count_key)
    redis.call('HINCRBY', inst_key, bucket_id, 1)
    return new_count
end

return -1
