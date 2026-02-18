// Package coordinator implements distributed coordination via Redis
// for connection pooling across multiple proxy instances.
//
// It provides:
//   - Atomic acquire/release of connection slots using Lua scripts
//   - Per-instance connection tracking for auditability
//   - Fallback mode when Redis is unavailable (local limits)
//   - Pub/Sub notifications for cross-instance queue wakeup
package coordinator

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/joao-brasil/poc-connection-pooling/internal/config"
	"github.com/joao-brasil/poc-connection-pooling/internal/metrics"
	"github.com/redis/go-redis/v9"
)

//go:embed lua/acquire.lua
var acquireLuaScript string

//go:embed lua/release.lua
var releaseLuaScript string

// ── Redis Key Patterns ──────────────────────────────────────────────────
const (
	keyBucketCount  = "proxy:bucket:%s:count"    // global connection count per bucket
	keyBucketMax    = "proxy:bucket:%s:max"       // max connections per bucket
	keyInstanceConn = "proxy:instance:%s:conns"   // hash: bucket_id → local count
	keyInstanceHB   = "proxy:instance:%s:heartbeat" // heartbeat key with TTL
	keyInstanceList = "proxy:instances"            // set of active instance IDs
	channelRelease  = "proxy:release:%s"           // Pub/Sub channel per bucket
)

// RedisCoordinator manages distributed connection limits via Redis.
type RedisCoordinator struct {
	client     redis.UniversalClient
	cfg        *config.Config
	instanceID string

	// Lua script SHA hashes (loaded once at startup).
	acquireSHA string
	releaseSHA string

	// fallback tracks whether Redis is unavailable and we're in local mode.
	fallbackMode atomic.Bool

	// fallbackCounts tracks local connection counts per bucket in fallback mode.
	fallbackMu     sync.Mutex
	fallbackCounts map[string]int

	// subscribers holds Pub/Sub subscriptions per bucket.
	subMu       sync.Mutex
	subscribers map[string]*redis.PubSub

	// lifecycle
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewRedisCoordinator creates and initializes the distributed coordinator.
func NewRedisCoordinator(ctx context.Context, cfg *config.Config) (*RedisCoordinator, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Redis.Addr,
		Password:     cfg.Redis.Password,
		DB:           cfg.Redis.DB,
		PoolSize:     cfg.Redis.PoolSize,
		DialTimeout:  cfg.Redis.DialTimeout,
		ReadTimeout:  cfg.Redis.ReadTimeout,
		WriteTimeout: cfg.Redis.WriteTimeout,
	})

	rc := &RedisCoordinator{
		client:         client,
		cfg:            cfg,
		instanceID:     cfg.Proxy.InstanceID,
		fallbackCounts: make(map[string]int),
		subscribers:    make(map[string]*redis.PubSub),
		stopCh:         make(chan struct{}),
	}

	// Test Redis connectivity.
	pingCtx, cancel := context.WithTimeout(ctx, cfg.Redis.DialTimeout)
	defer cancel()

	if err := client.Ping(pingCtx).Err(); err != nil {
		if cfg.Fallback.Enabled {
			log.Printf("[coordinator] Redis unavailable (%v), starting in fallback mode", err)
			rc.fallbackMode.Store(true)
			metrics.RedisOperations.WithLabelValues("ping", "error").Inc()
			return rc, nil
		}
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}
	metrics.RedisOperations.WithLabelValues("ping", "ok").Inc()
	log.Printf("[coordinator] Redis connected: %s", cfg.Redis.Addr)

	// Load Lua scripts.
	if err := rc.loadScripts(ctx); err != nil {
		return nil, fmt.Errorf("loading lua scripts: %w", err)
	}

	// Register bucket max connections.
	if err := rc.initBucketLimits(ctx); err != nil {
		return nil, fmt.Errorf("initializing bucket limits: %w", err)
	}

	// Register this instance.
	if err := rc.registerInstance(ctx); err != nil {
		return nil, fmt.Errorf("registering instance: %w", err)
	}

	log.Printf("[coordinator] Initialized: instance=%s, %d buckets registered",
		rc.instanceID, len(cfg.Buckets))

	return rc, nil
}

// loadScripts loads the Lua scripts into Redis and caches their SHA hashes.
func (rc *RedisCoordinator) loadScripts(ctx context.Context) error {
	sha, err := rc.client.ScriptLoad(ctx, acquireLuaScript).Result()
	if err != nil {
		return fmt.Errorf("loading acquire.lua: %w", err)
	}
	rc.acquireSHA = sha

	sha, err = rc.client.ScriptLoad(ctx, releaseLuaScript).Result()
	if err != nil {
		return fmt.Errorf("loading release.lua: %w", err)
	}
	rc.releaseSHA = sha

	log.Printf("[coordinator] Lua scripts loaded (acquire=%s..., release=%s...)",
		rc.acquireSHA[:8], rc.releaseSHA[:8])
	return nil
}

// initBucketLimits sets the max connection count for each bucket in Redis.
func (rc *RedisCoordinator) initBucketLimits(ctx context.Context) error {
	pipe := rc.client.Pipeline()
	for _, b := range rc.cfg.Buckets {
		maxKey := fmt.Sprintf(keyBucketMax, b.ID)
		pipe.Set(ctx, maxKey, b.MaxConnections, 0)

		// Initialize count key if it doesn't exist.
		countKey := fmt.Sprintf(keyBucketCount, b.ID)
		pipe.SetNX(ctx, countKey, 0, 0)
	}
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("pipeline exec: %w", err)
	}
	return nil
}

// registerInstance adds this instance to the active instances set.
func (rc *RedisCoordinator) registerInstance(ctx context.Context) error {
	pipe := rc.client.Pipeline()
	pipe.SAdd(ctx, keyInstanceList, rc.instanceID)

	// Initialize per-instance connection hash.
	instKey := fmt.Sprintf(keyInstanceConn, rc.instanceID)
	for _, b := range rc.cfg.Buckets {
		pipe.HSetNX(ctx, instKey, b.ID, 0)
	}

	_, err := pipe.Exec(ctx)
	return err
}

// ── Acquire / Release ───────────────────────────────────────────────────

// Acquire atomically increments the global connection count for a bucket.
// Returns nil if the slot was acquired, or an error if at capacity or Redis fails.
func (rc *RedisCoordinator) Acquire(ctx context.Context, bucketID string) error {
	if rc.fallbackMode.Load() {
		return rc.acquireFallback(bucketID)
	}

	countKey := fmt.Sprintf(keyBucketCount, bucketID)
	maxKey := fmt.Sprintf(keyBucketMax, bucketID)
	instKey := fmt.Sprintf(keyInstanceConn, rc.instanceID)

	result, err := rc.client.EvalSha(ctx, rc.acquireSHA,
		[]string{countKey, maxKey, instKey},
		bucketID, rc.instanceID,
	).Int64()

	if err != nil {
		metrics.RedisOperations.WithLabelValues("acquire", "error").Inc()
		// If Redis fails, try fallback.
		if rc.cfg.Fallback.Enabled {
			log.Printf("[coordinator] Redis acquire failed (%v), falling back to local", err)
			rc.enterFallback()
			return rc.acquireFallback(bucketID)
		}
		return fmt.Errorf("redis acquire: %w", err)
	}

	metrics.RedisOperations.WithLabelValues("acquire", "ok").Inc()

	if result == -1 {
		return fmt.Errorf("bucket %s at max capacity", bucketID)
	}
	if result == -2 {
		return fmt.Errorf("bucket %s max not configured in Redis", bucketID)
	}

	return nil
}

// Release atomically decrements the global connection count for a bucket
// and publishes a notification for waiting instances.
func (rc *RedisCoordinator) Release(ctx context.Context, bucketID string) error {
	if rc.fallbackMode.Load() {
		rc.releaseFallback(bucketID)
		return nil
	}

	countKey := fmt.Sprintf(keyBucketCount, bucketID)
	instKey := fmt.Sprintf(keyInstanceConn, rc.instanceID)
	channel := fmt.Sprintf(channelRelease, bucketID)

	_, err := rc.client.EvalSha(ctx, rc.releaseSHA,
		[]string{countKey, instKey},
		bucketID, channel,
	).Int64()

	if err != nil {
		metrics.RedisOperations.WithLabelValues("release", "error").Inc()
		if rc.cfg.Fallback.Enabled {
			rc.enterFallback()
			rc.releaseFallback(bucketID)
			return nil
		}
		return fmt.Errorf("redis release: %w", err)
	}

	metrics.RedisOperations.WithLabelValues("release", "ok").Inc()
	return nil
}

// ── Pub/Sub for Cross-Instance Notifications ────────────────────────────

// Subscribe creates a Pub/Sub subscription for a bucket's release notifications.
// Returns a channel that receives the bucket ID whenever a connection is released
// by any instance.
func (rc *RedisCoordinator) Subscribe(ctx context.Context, bucketID string) (<-chan string, error) {
	if rc.fallbackMode.Load() {
		// In fallback mode, return a closed channel (no cross-instance coordination).
		ch := make(chan string)
		close(ch)
		return ch, nil
	}

	channel := fmt.Sprintf(channelRelease, bucketID)
	sub := rc.client.Subscribe(ctx, channel)

	rc.subMu.Lock()
	rc.subscribers[bucketID] = sub
	rc.subMu.Unlock()

	notifyCh := make(chan string, 16)

	rc.wg.Add(1)
	go func() {
		defer rc.wg.Done()
		defer close(notifyCh)

		ch := sub.Channel()
		for {
			select {
			case <-rc.stopCh:
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				select {
				case notifyCh <- msg.Payload:
				default:
					// Drop if consumer is slow (anti-thundering-herd).
				}
			}
		}
	}()

	return notifyCh, nil
}

// ── Fallback Mode ───────────────────────────────────────────────────────

func (rc *RedisCoordinator) enterFallback() {
	if rc.fallbackMode.CompareAndSwap(false, true) {
		log.Printf("[coordinator] Entering fallback mode (local limits)")
		metrics.ConnectionErrors.WithLabelValues("coordinator", "fallback_entered").Inc()
	}
}

// ExitFallback attempts to reconnect to Redis and leave fallback mode.
func (rc *RedisCoordinator) ExitFallback(ctx context.Context) error {
	if err := rc.client.Ping(ctx).Err(); err != nil {
		return err
	}

	// Re-load scripts (may have been flushed).
	if err := rc.loadScripts(ctx); err != nil {
		return err
	}

	// Reconcile: sync local counts to Redis.
	if err := rc.reconcileCounts(ctx); err != nil {
		log.Printf("[coordinator] Reconciliation failed: %v", err)
		// Don't exit fallback if reconciliation fails.
		return err
	}

	rc.fallbackMode.Store(false)
	log.Printf("[coordinator] Exited fallback mode, Redis reconnected")
	metrics.ConnectionErrors.WithLabelValues("coordinator", "fallback_exited").Inc()
	return nil
}

// IsFallback returns true if the coordinator is in fallback mode.
func (rc *RedisCoordinator) IsFallback() bool {
	return rc.fallbackMode.Load()
}

func (rc *RedisCoordinator) acquireFallback(bucketID string) error {
	rc.fallbackMu.Lock()
	defer rc.fallbackMu.Unlock()

	localMax := rc.localLimit(bucketID)
	current := rc.fallbackCounts[bucketID]

	if current >= localMax {
		return fmt.Errorf("bucket %s at local fallback limit (%d/%d)",
			bucketID, current, localMax)
	}

	rc.fallbackCounts[bucketID] = current + 1
	return nil
}

func (rc *RedisCoordinator) releaseFallback(bucketID string) {
	rc.fallbackMu.Lock()
	defer rc.fallbackMu.Unlock()

	if rc.fallbackCounts[bucketID] > 0 {
		rc.fallbackCounts[bucketID]--
	}
}

// localLimit calculates the per-instance connection limit for fallback mode.
func (rc *RedisCoordinator) localLimit(bucketID string) int {
	for _, b := range rc.cfg.Buckets {
		if b.ID == bucketID {
			divisor := rc.cfg.Fallback.LocalLimitDivisor
			if divisor <= 0 {
				divisor = 3
			}
			limit := b.MaxConnections / divisor
			if limit < 1 {
				limit = 1
			}
			return limit
		}
	}
	return 1
}

// reconcileCounts syncs local fallback counts to Redis after reconnection.
func (rc *RedisCoordinator) reconcileCounts(ctx context.Context) error {
	rc.fallbackMu.Lock()
	counts := make(map[string]int, len(rc.fallbackCounts))
	for k, v := range rc.fallbackCounts {
		counts[k] = v
	}
	rc.fallbackMu.Unlock()

	pipe := rc.client.Pipeline()
	instKey := fmt.Sprintf(keyInstanceConn, rc.instanceID)

	for bucketID, count := range counts {
		pipe.HSet(ctx, instKey, bucketID, count)
	}

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("reconcile pipeline: %w", err)
	}

	log.Printf("[coordinator] Reconciled %d bucket counts to Redis", len(counts))
	return nil
}

// ── Query Methods ───────────────────────────────────────────────────────

// GlobalCount returns the current global connection count for a bucket.
func (rc *RedisCoordinator) GlobalCount(ctx context.Context, bucketID string) (int, error) {
	if rc.fallbackMode.Load() {
		rc.fallbackMu.Lock()
		defer rc.fallbackMu.Unlock()
		return rc.fallbackCounts[bucketID], nil
	}

	countKey := fmt.Sprintf(keyBucketCount, bucketID)
	val, err := rc.client.Get(ctx, countKey).Int()
	if err == redis.Nil {
		return 0, nil
	}
	return val, err
}

// InstanceCounts returns the per-bucket connection counts for a specific instance.
func (rc *RedisCoordinator) InstanceCounts(ctx context.Context, instanceID string) (map[string]int, error) {
	instKey := fmt.Sprintf(keyInstanceConn, instanceID)
	result, err := rc.client.HGetAll(ctx, instKey).Result()
	if err != nil {
		return nil, err
	}

	counts := make(map[string]int, len(result))
	for k, v := range result {
		var n int
		fmt.Sscanf(v, "%d", &n)
		counts[k] = n
	}
	return counts, nil
}

// ActiveInstances returns the set of active instance IDs.
func (rc *RedisCoordinator) ActiveInstances(ctx context.Context) ([]string, error) {
	return rc.client.SMembers(ctx, keyInstanceList).Result()
}

// ── Lifecycle ───────────────────────────────────────────────────────────

// Close shuts down the coordinator, unregisters the instance, and closes Redis.
func (rc *RedisCoordinator) Close(ctx context.Context) error {
	close(rc.stopCh)

	// Close all Pub/Sub subscriptions.
	rc.subMu.Lock()
	for _, sub := range rc.subscribers {
		sub.Close()
	}
	rc.subscribers = nil
	rc.subMu.Unlock()

	rc.wg.Wait()

	// Unregister instance.
	if !rc.fallbackMode.Load() {
		rc.client.SRem(ctx, keyInstanceList, rc.instanceID)
		instKey := fmt.Sprintf(keyInstanceConn, rc.instanceID)
		rc.client.Del(ctx, instKey)
		hbKey := fmt.Sprintf(keyInstanceHB, rc.instanceID)
		rc.client.Del(ctx, hbKey)
	}

	log.Printf("[coordinator] Instance %s unregistered", rc.instanceID)
	return rc.client.Close()
}

// Client returns the underlying Redis client (for heartbeat and other internal use).
func (rc *RedisCoordinator) Client() redis.UniversalClient {
	return rc.client
}

// InstanceID returns this coordinator's instance ID.
func (rc *RedisCoordinator) InstanceID() string {
	return rc.instanceID
}
