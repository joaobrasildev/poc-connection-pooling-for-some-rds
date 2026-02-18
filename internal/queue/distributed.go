// Package queue provides distributed queue mechanisms for cross-instance
// coordination of connection waiting. It wraps the coordinator's Pub/Sub
// notifications and the distributed semaphore to provide a unified waiting
// interface for the connection pool.
//
// Phase 4 additions: circuit breaker (max queue size), per-bucket metrics,
// and graceful rejection with TDS error support.
package queue

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/joao-brasil/poc-connection-pooling/internal/coordinator"
	"github.com/joao-brasil/poc-connection-pooling/internal/metrics"
)

// DistributedQueue manages distributed wait queues for all buckets.
// When a local pool is at global capacity, callers wait on the distributed
// semaphore. When any proxy instance releases a connection, all waiting
// instances are notified via Pub/Sub so one of them can acquire the slot.
type DistributedQueue struct {
	coordinator *coordinator.RedisCoordinator
	semaphore   *coordinator.Semaphore

	// per-bucket queue depth tracking
	mu     sync.Mutex
	depths map[string]int

	timeout      time.Duration // max wait time per request
	maxQueueSize int           // max queue depth per bucket (0 = unlimited)
}

// NewDistributedQueue creates a new distributed queue backed by the coordinator.
func NewDistributedQueue(rc *coordinator.RedisCoordinator, timeout time.Duration, maxQueueSize int) *DistributedQueue {
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &DistributedQueue{
		coordinator:  rc,
		semaphore:    coordinator.NewSemaphore(rc),
		depths:       make(map[string]int),
		timeout:      timeout,
		maxQueueSize: maxQueueSize,
	}
}

// Acquire tries to get a distributed slot for the given bucket.
// It first attempts an immediate acquire. If that fails (bucket at capacity),
// it checks the circuit breaker (max queue size) and enters the distributed
// wait queue using the semaphore.
//
// Returns nil if a slot was acquired, or an error on timeout/cancellation/rejection.
// The error type can be checked to determine the appropriate TDS error to send:
//   - ErrQueueFull: circuit breaker triggered (queue at max capacity)
//   - ErrQueueTimeout: waited but timed out
//   - context.Canceled / context.DeadlineExceeded: client disconnected
func (dq *DistributedQueue) Acquire(ctx context.Context, bucketID string) error {
	// Fast path: try non-blocking acquire.
	if err := dq.semaphore.TryAcquire(ctx, bucketID); err == nil {
		metrics.ConnectionsTotal.WithLabelValues(bucketID, "acquired").Inc()
		return nil
	}

	// Circuit breaker: reject immediately if queue is already at max depth.
	if dq.maxQueueSize > 0 {
		currentDepth := dq.getDepth(bucketID)
		if currentDepth >= dq.maxQueueSize {
			metrics.ConnectionsTotal.WithLabelValues(bucketID, "rejected_queue_full").Inc()
			log.Printf("[dqueue] Circuit breaker: rejecting request for bucket %s (queue depth=%d, max=%d)",
				bucketID, currentDepth, dq.maxQueueSize)
			return &QueueError{
				BucketID: bucketID,
				Kind:     QueueErrorFull,
				Depth:    currentDepth,
				MaxSize:  dq.maxQueueSize,
			}
		}
	}

	// Slow path: enter distributed wait queue.
	dq.incrementDepth(bucketID)
	defer dq.decrementDepth(bucketID)

	log.Printf("[dqueue] Entering distributed wait for bucket %s (depth=%d, timeout=%s)",
		bucketID, dq.getDepth(bucketID), dq.timeout)

	start := time.Now()
	err := dq.semaphore.Wait(ctx, bucketID, dq.timeout)
	dur := time.Since(start)

	if err != nil {
		// Classify the error.
		if ctx.Err() != nil {
			metrics.ConnectionsTotal.WithLabelValues(bucketID, "cancelled").Inc()
			log.Printf("[dqueue] Wait cancelled for bucket %s after %v: %v", bucketID, dur, err)
			return ctx.Err()
		}
		metrics.ConnectionsTotal.WithLabelValues(bucketID, "timeout").Inc()
		log.Printf("[dqueue] Wait timed out for bucket %s after %v: %v", bucketID, dur, err)
		return &QueueError{
			BucketID: bucketID,
			Kind:     QueueErrorTimeout,
			WaitTime: dur,
			Timeout:  dq.timeout,
		}
	}

	metrics.ConnectionsTotal.WithLabelValues(bucketID, "acquired_after_wait").Inc()
	log.Printf("[dqueue] Acquired slot for bucket %s after %v wait", bucketID, dur)
	return nil
}

// Release notifies the distributed queue that a connection was released.
// This is handled internally by the coordinator's Lua script (PUBLISH).
// Calling this method explicitly ensures the coordinator release is invoked.
func (dq *DistributedQueue) Release(ctx context.Context, bucketID string) error {
	return dq.coordinator.Release(ctx, bucketID)
}

// Depth returns the current distributed wait queue depth for a bucket.
func (dq *DistributedQueue) Depth(bucketID string) int {
	return dq.getDepth(bucketID)
}

// ── Queue Error Types ───────────────────────────────────────────────────

// QueueErrorKind classifies the type of queue error.
type QueueErrorKind int

const (
	// QueueErrorTimeout means the request waited the full timeout period.
	QueueErrorTimeout QueueErrorKind = iota
	// QueueErrorFull means the queue is at max capacity (circuit breaker).
	QueueErrorFull
)

// QueueError provides structured error information for queue failures.
type QueueError struct {
	BucketID string
	Kind     QueueErrorKind
	Depth    int           // current queue depth (for QueueErrorFull)
	MaxSize  int           // max queue size (for QueueErrorFull)
	WaitTime time.Duration // how long the request waited (for QueueErrorTimeout)
	Timeout  time.Duration // configured timeout (for QueueErrorTimeout)
}

func (e *QueueError) Error() string {
	switch e.Kind {
	case QueueErrorFull:
		return fmt.Sprintf("queue full for bucket %s (depth=%d, max=%d)",
			e.BucketID, e.Depth, e.MaxSize)
	case QueueErrorTimeout:
		return fmt.Sprintf("queue timeout for bucket %s (waited=%v, timeout=%v)",
			e.BucketID, e.WaitTime, e.Timeout)
	default:
		return fmt.Sprintf("queue error for bucket %s", e.BucketID)
	}
}

// IsQueueFull checks if the error is a circuit breaker rejection.
func IsQueueFull(err error) bool {
	if qe, ok := err.(*QueueError); ok {
		return qe.Kind == QueueErrorFull
	}
	return false
}

// IsQueueTimeout checks if the error is a queue timeout.
func IsQueueTimeout(err error) bool {
	if qe, ok := err.(*QueueError); ok {
		return qe.Kind == QueueErrorTimeout
	}
	return false
}

// ── Internal helpers ─────────────────────────────────────────────────────

func (dq *DistributedQueue) incrementDepth(bucketID string) {
	dq.mu.Lock()
	dq.depths[bucketID]++
	depth := dq.depths[bucketID]
	dq.mu.Unlock()
	metrics.QueueLength.WithLabelValues(bucketID).Set(float64(depth))
}

func (dq *DistributedQueue) decrementDepth(bucketID string) {
	dq.mu.Lock()
	dq.depths[bucketID]--
	if dq.depths[bucketID] < 0 {
		dq.depths[bucketID] = 0
	}
	depth := dq.depths[bucketID]
	dq.mu.Unlock()
	metrics.QueueLength.WithLabelValues(bucketID).Set(float64(depth))
}

func (dq *DistributedQueue) getDepth(bucketID string) int {
	dq.mu.Lock()
	defer dq.mu.Unlock()
	return dq.depths[bucketID]
}
