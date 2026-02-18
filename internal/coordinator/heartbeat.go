package coordinator

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/joao-brasil/poc-connection-pooling/internal/metrics"
)

// Heartbeat periodically refreshes this instance's presence in Redis
// and detects/cleans up dead instances whose connections were not released.
type Heartbeat struct {
	coordinator *RedisCoordinator
	interval    time.Duration
	ttl         time.Duration
	stopCh      chan struct{}
}

// NewHeartbeat creates a heartbeat worker for the given coordinator.
func NewHeartbeat(rc *RedisCoordinator) *Heartbeat {
	interval := rc.cfg.Redis.HeartbeatInterval
	if interval == 0 {
		interval = 10 * time.Second
	}
	ttl := rc.cfg.Redis.HeartbeatTTL
	if ttl == 0 {
		ttl = 30 * time.Second
	}

	return &Heartbeat{
		coordinator: rc,
		interval:    interval,
		ttl:         ttl,
		stopCh:      make(chan struct{}),
	}
}

// Start begins the heartbeat loop in a background goroutine.
func (hb *Heartbeat) Start(ctx context.Context) {
	hb.coordinator.wg.Add(1)
	go hb.loop(ctx)
	log.Printf("[heartbeat] Started: interval=%s, ttl=%s, instance=%s",
		hb.interval, hb.ttl, hb.coordinator.instanceID)
}

// Stop signals the heartbeat loop to stop.
func (hb *Heartbeat) Stop() {
	close(hb.stopCh)
}

// loop runs the periodic heartbeat and dead-instance cleanup.
func (hb *Heartbeat) loop(ctx context.Context) {
	defer hb.coordinator.wg.Done()

	// Send initial heartbeat immediately.
	hb.sendHeartbeat(ctx)

	ticker := time.NewTicker(hb.interval)
	defer ticker.Stop()

	// Run cleanup less frequently (every 3 intervals).
	cleanupCounter := 0

	for {
		select {
		case <-hb.stopCh:
			return
		case <-hb.coordinator.stopCh:
			return
		case <-ticker.C:
			if hb.coordinator.IsFallback() {
				// Try to reconnect to Redis.
				if err := hb.coordinator.ExitFallback(ctx); err != nil {
					continue
				}
			}

			hb.sendHeartbeat(ctx)

			cleanupCounter++
			if cleanupCounter%3 == 0 {
				hb.cleanupDeadInstances(ctx)
			}
		}
	}
}

// sendHeartbeat refreshes this instance's heartbeat key with a TTL.
func (hb *Heartbeat) sendHeartbeat(ctx context.Context) {
	if hb.coordinator.IsFallback() {
		return
	}

	hbKey := fmt.Sprintf(keyInstanceHB, hb.coordinator.instanceID)
	err := hb.coordinator.client.Set(ctx, hbKey, time.Now().Unix(), hb.ttl).Err()
	if err != nil {
		log.Printf("[heartbeat] Failed to send heartbeat: %v", err)
		metrics.RedisOperations.WithLabelValues("heartbeat", "error").Inc()
		return
	}

	metrics.InstanceHeartbeat.WithLabelValues(hb.coordinator.instanceID).Set(1)
	metrics.RedisOperations.WithLabelValues("heartbeat", "ok").Inc()
}

// cleanupDeadInstances checks for instances whose heartbeat has expired
// and reconciles their orphaned connection counts.
func (hb *Heartbeat) cleanupDeadInstances(ctx context.Context) {
	if hb.coordinator.IsFallback() {
		return
	}

	// Get all registered instances.
	instances, err := hb.coordinator.client.SMembers(ctx, keyInstanceList).Result()
	if err != nil {
		log.Printf("[heartbeat] Failed to list instances: %v", err)
		return
	}

	for _, instID := range instances {
		if instID == hb.coordinator.instanceID {
			continue // skip ourselves
		}

		// Check if the instance's heartbeat is still alive.
		hbKey := fmt.Sprintf(keyInstanceHB, instID)
		exists, err := hb.coordinator.client.Exists(ctx, hbKey).Result()
		if err != nil {
			continue
		}

		if exists > 0 {
			continue // instance is alive
		}

		// Instance is dead — clean up its orphaned connections.
		log.Printf("[heartbeat] Instance %s appears dead (no heartbeat), cleaning up", instID)
		hb.cleanupInstance(ctx, instID)
	}
}

// cleanupInstance removes an instance's connection counts from the global totals.
func (hb *Heartbeat) cleanupInstance(ctx context.Context, deadInstanceID string) {
	instKey := fmt.Sprintf(keyInstanceConn, deadInstanceID)

	// Read the dead instance's per-bucket connection counts.
	counts, err := hb.coordinator.client.HGetAll(ctx, instKey).Result()
	if err != nil {
		log.Printf("[heartbeat] Failed to read counts for dead instance %s: %v", deadInstanceID, err)
		return
	}

	// Subtract the dead instance's counts from global counters.
	pipe := hb.coordinator.client.Pipeline()
	totalRecovered := 0

	for bucketID, countStr := range counts {
		count, err := strconv.Atoi(countStr)
		if err != nil || count <= 0 {
			continue
		}

		countKey := fmt.Sprintf(keyBucketCount, bucketID)
		pipe.DecrBy(ctx, countKey, int64(count))
		totalRecovered += count
	}

	// Remove the dead instance's data.
	pipe.Del(ctx, instKey)
	pipe.SRem(ctx, keyInstanceList, deadInstanceID)

	_, err = pipe.Exec(ctx)
	if err != nil {
		log.Printf("[heartbeat] Failed to cleanup dead instance %s: %v", deadInstanceID, err)
		return
	}

	if totalRecovered > 0 {
		log.Printf("[heartbeat] Cleaned up dead instance %s: recovered %d connection slots",
			deadInstanceID, totalRecovered)
		metrics.ConnectionErrors.WithLabelValues("coordinator", "dead_instance_cleanup").Inc()
	}

	// Ensure global counts don't go below zero.
	for bucketID := range counts {
		countKey := fmt.Sprintf(keyBucketCount, bucketID)
		val, err := hb.coordinator.client.Get(ctx, countKey).Int64()
		if err == nil && val < 0 {
			hb.coordinator.client.Set(ctx, countKey, 0, 0)
			log.Printf("[heartbeat] Corrected negative count for bucket %s", bucketID)
		}
	}
}
