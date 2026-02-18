package coordinator

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/joao-brasil/poc-connection-pooling/internal/metrics"
)

// ── Distributed Semaphore ───────────────────────────────────────────────
//
// The semaphore provides a distributed waiting mechanism for connection
// acquisition. When the global pool for a bucket is full, callers wait
// on the semaphore until a connection is released by any proxy instance.
//
// It combines:
//   - Redis Pub/Sub for instant cross-instance notifications
//   - Polling fallback to handle missed Pub/Sub messages
//   - Timeout to prevent indefinite waiting

// Semaphore provides distributed waiting for connection availability.
type Semaphore struct {
	coordinator *RedisCoordinator
}

// NewSemaphore creates a new distributed semaphore.
func NewSemaphore(rc *RedisCoordinator) *Semaphore {
	return &Semaphore{coordinator: rc}
}

// Wait blocks until a connection slot becomes available for the given bucket,
// then atomically acquires it. Returns an error if the context expires or
// the wait times out.
func (s *Semaphore) Wait(ctx context.Context, bucketID string, timeout time.Duration) error {
	// Fast path: try immediate acquire.
	if err := s.coordinator.Acquire(ctx, bucketID); err == nil {
		return nil
	}

	start := time.Now()
	log.Printf("[semaphore] Waiting for connection slot on bucket %s (timeout=%s)", bucketID, timeout)

	// Subscribe to release notifications for this bucket.
	notifyCh, err := s.coordinator.Subscribe(ctx, bucketID)
	if err != nil {
		// Can't subscribe — fall back to polling.
		return s.waitPolling(ctx, bucketID, timeout)
	}

	// Set up timeout.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// Also poll periodically as a safety net (in case Pub/Sub messages are lost).
	pollTicker := time.NewTicker(500 * time.Millisecond)
	defer pollTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			metrics.ConnectionsTotal.WithLabelValues(bucketID, "semaphore_cancelled").Inc()
			return ctx.Err()

		case <-timer.C:
			metrics.ConnectionsTotal.WithLabelValues(bucketID, "semaphore_timeout").Inc()
			return fmt.Errorf("semaphore timeout (%v) for bucket %s", timeout, bucketID)

		case _, ok := <-notifyCh:
			if !ok {
				// Channel closed, switch to polling.
				return s.waitPolling(ctx, bucketID, timeout-time.Since(start))
			}
			// A connection was released — try to acquire.
			if err := s.coordinator.Acquire(ctx, bucketID); err == nil {
				dur := time.Since(start)
				metrics.QueueWaitDuration.WithLabelValues(bucketID).Observe(dur.Seconds())
				log.Printf("[semaphore] Acquired slot on bucket %s after %v", bucketID, dur)
				return nil
			}
			// Someone else got it first — keep waiting.

		case <-pollTicker.C:
			// Periodic retry in case we missed a notification.
			if err := s.coordinator.Acquire(ctx, bucketID); err == nil {
				dur := time.Since(start)
				metrics.QueueWaitDuration.WithLabelValues(bucketID).Observe(dur.Seconds())
				log.Printf("[semaphore] Acquired slot on bucket %s after %v (poll)", bucketID, dur)
				return nil
			}
		}
	}
}

// waitPolling is a fallback that polls Redis for slot availability.
func (s *Semaphore) waitPolling(ctx context.Context, bucketID string, remaining time.Duration) error {
	if remaining <= 0 {
		return fmt.Errorf("semaphore timeout for bucket %s", bucketID)
	}

	start := time.Now()
	timer := time.NewTimer(remaining)
	defer timer.Stop()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			metrics.ConnectionsTotal.WithLabelValues(bucketID, "semaphore_timeout").Inc()
			return fmt.Errorf("semaphore timeout (%v) for bucket %s", remaining, bucketID)
		case <-ticker.C:
			if err := s.coordinator.Acquire(ctx, bucketID); err == nil {
				dur := time.Since(start)
				metrics.QueueWaitDuration.WithLabelValues(bucketID).Observe(dur.Seconds())
				return nil
			}
		}
	}
}

// TryAcquire attempts a single non-blocking acquire.
func (s *Semaphore) TryAcquire(ctx context.Context, bucketID string) error {
	err := s.coordinator.Acquire(ctx, bucketID)
	if err != nil {
		metrics.RedisOperations.WithLabelValues("try_acquire", "rejected").Inc()
	} else {
		metrics.RedisOperations.WithLabelValues("try_acquire", "ok").Inc()
	}
	return err
}
