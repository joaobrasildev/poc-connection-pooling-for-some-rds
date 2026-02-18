package pool

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joao-brasil/poc-connection-pooling/internal/metrics"
	"github.com/joao-brasil/poc-connection-pooling/pkg/bucket"
	_ "github.com/microsoft/go-mssqldb"
)

// BucketPool manages a pool of SQL Server connections for a single bucket.
// It provides acquire/release semantics with configurable limits, a warm pool
// of idle connections, eviction of stale connections, and health checking.
type BucketPool struct {
	mu sync.Mutex

	bucket *bucket.Bucket

	// idle holds connections available for reuse, most recently used first.
	idle []*PooledConn

	// active tracks connections currently in use (keyed by connection ID).
	active map[uint64]*PooledConn

	// nextID is an atomic counter for assigning unique connection IDs.
	nextID atomic.Uint64

	// closed indicates whether the pool has been shut down.
	closed bool

	// waiters is a channel-based queue for callers waiting for a connection.
	// Each waiter sends a channel that will receive the allocated connection.
	waiters []chan *PooledConn

	// notify is used to signal that a connection has been returned to the pool.
	notify chan struct{}

	// stopCh signals background goroutines to stop.
	stopCh chan struct{}

	// wg tracks background goroutines.
	wg sync.WaitGroup
}

// NewBucketPool creates a new pool for the given bucket and eagerly opens min_idle connections.
func NewBucketPool(ctx context.Context, b *bucket.Bucket) (*BucketPool, error) {
	bp := &BucketPool{
		bucket:  b,
		idle:    make([]*PooledConn, 0, b.MaxConnections),
		active:  make(map[uint64]*PooledConn),
		notify:  make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
	}

	// Eagerly create min_idle connections (warm pool).
	for i := 0; i < b.MinIdle; i++ {
		conn, err := bp.createConn(ctx)
		if err != nil {
			log.Printf("[pool] WARNING: bucket %s — failed to create warm connection %d/%d: %v",
				b.ID, i+1, b.MinIdle, err)
			continue
		}
		bp.idle = append(bp.idle, conn)
	}

	bp.updateMetrics()
	log.Printf("[pool] Bucket %s — pool initialized: %d idle, max=%d",
		b.ID, len(bp.idle), b.MaxConnections)

	// Start background maintenance.
	bp.wg.Add(1)
	go bp.maintenanceLoop()

	return bp, nil
}

// Acquire obtains a connection from the pool. If no connection is available
// and the pool is at max capacity, the caller blocks until a connection is
// released or the context expires.
func (bp *BucketPool) Acquire(ctx context.Context) (*PooledConn, error) {
	start := time.Now()

	bp.mu.Lock()
	if bp.closed {
		bp.mu.Unlock()
		return nil, fmt.Errorf("pool closed for bucket %s", bp.bucket.ID)
	}

	// Try to get an idle connection.
	if conn := bp.popIdle(); conn != nil {
		bp.active[conn.id] = conn
		conn.markAcquired()
		bp.updateMetrics()
		bp.mu.Unlock()
		metrics.ConnectionsTotal.WithLabelValues(bp.bucket.ID, "acquired").Inc()
		return conn, nil
	}

	// If under max, create a new connection.
	totalCount := len(bp.idle) + len(bp.active)
	if totalCount < bp.bucket.MaxConnections {
		bp.mu.Unlock()
		conn, err := bp.createConn(ctx)
		if err != nil {
			metrics.ConnectionErrors.WithLabelValues(bp.bucket.ID, "create_failed").Inc()
			return nil, fmt.Errorf("creating connection for bucket %s: %w", bp.bucket.ID, err)
		}
		conn.markAcquired()
		bp.mu.Lock()
		bp.active[conn.id] = conn
		bp.updateMetrics()
		bp.mu.Unlock()
		metrics.ConnectionsTotal.WithLabelValues(bp.bucket.ID, "acquired").Inc()
		return conn, nil
	}

	// Pool is full — enter wait queue.
	waiterCh := make(chan *PooledConn, 1)
	bp.waiters = append(bp.waiters, waiterCh)
	metrics.QueueLength.WithLabelValues(bp.bucket.ID).Set(float64(len(bp.waiters)))
	bp.mu.Unlock()

	log.Printf("[pool] Bucket %s — connection queue entered, position=%d",
		bp.bucket.ID, len(bp.waiters))

	// Wait for either a connection, context cancellation, or queue timeout.
	queueTimeout := bp.bucket.QueueTimeout
	if queueTimeout == 0 {
		queueTimeout = 30 * time.Second
	}
	timer := time.NewTimer(queueTimeout)
	defer timer.Stop()

	select {
	case conn := <-waiterCh:
		if conn == nil {
			metrics.ConnectionsTotal.WithLabelValues(bp.bucket.ID, "queue_error").Inc()
			return nil, fmt.Errorf("pool closed while waiting for bucket %s", bp.bucket.ID)
		}
		metrics.QueueWaitDuration.WithLabelValues(bp.bucket.ID).Observe(time.Since(start).Seconds())
		metrics.ConnectionsTotal.WithLabelValues(bp.bucket.ID, "acquired").Inc()
		return conn, nil

	case <-timer.C:
		bp.removeWaiter(waiterCh)
		metrics.ConnectionsTotal.WithLabelValues(bp.bucket.ID, "timeout").Inc()
		metrics.QueueWaitDuration.WithLabelValues(bp.bucket.ID).Observe(time.Since(start).Seconds())
		return nil, fmt.Errorf("queue timeout (%v) for bucket %s", queueTimeout, bp.bucket.ID)

	case <-ctx.Done():
		bp.removeWaiter(waiterCh)
		metrics.ConnectionsTotal.WithLabelValues(bp.bucket.ID, "cancelled").Inc()
		return nil, ctx.Err()
	}
}

// Release returns a connection back to the pool. It runs sp_reset_connection
// to clean session state before making it available for reuse.
func (bp *BucketPool) Release(conn *PooledConn) {
	if conn == nil {
		return
	}

	bp.mu.Lock()
	if bp.closed {
		bp.mu.Unlock()
		conn.Close()
		return
	}
	delete(bp.active, conn.id)
	bp.mu.Unlock()

	// Reset session state so the connection is safe for reuse.
	if err := bp.resetConnection(conn); err != nil {
		log.Printf("[pool] Bucket %s — sp_reset_connection failed on conn %d, closing: %v",
			bp.bucket.ID, conn.id, err)
		conn.Close()
		metrics.ConnectionErrors.WithLabelValues(bp.bucket.ID, "reset_failed").Inc()
		bp.mu.Lock()
		bp.updateMetrics()
		bp.mu.Unlock()
		return
	}

	conn.markIdle()

	bp.mu.Lock()
	// Hand off to a waiter if one is queued.
	if len(bp.waiters) > 0 {
		waiterCh := bp.waiters[0]
		bp.waiters = bp.waiters[1:]
		metrics.QueueLength.WithLabelValues(bp.bucket.ID).Set(float64(len(bp.waiters)))
		conn.markAcquired()
		bp.active[conn.id] = conn
		bp.updateMetrics()
		bp.mu.Unlock()
		waiterCh <- conn
		metrics.ConnectionsTotal.WithLabelValues(bp.bucket.ID, "released").Inc()
		return
	}

	bp.idle = append(bp.idle, conn)
	bp.updateMetrics()
	bp.mu.Unlock()
	metrics.ConnectionsTotal.WithLabelValues(bp.bucket.ID, "released").Inc()
}

// Discard removes a connection from the pool permanently (e.g. on error).
func (bp *BucketPool) Discard(conn *PooledConn) {
	if conn == nil {
		return
	}
	bp.mu.Lock()
	delete(bp.active, conn.id)
	bp.updateMetrics()
	bp.mu.Unlock()
	conn.Close()
	metrics.ConnectionErrors.WithLabelValues(bp.bucket.ID, "discarded").Inc()
}

// Close shuts down the pool, closing all connections and notifying waiters.
func (bp *BucketPool) Close() error {
	bp.mu.Lock()
	if bp.closed {
		bp.mu.Unlock()
		return nil
	}
	bp.closed = true

	// Signal background goroutines to stop.
	close(bp.stopCh)

	// Notify all waiters that the pool is closing.
	for _, w := range bp.waiters {
		close(w)
	}
	bp.waiters = nil

	// Close all idle connections.
	for _, c := range bp.idle {
		c.Close()
	}
	bp.idle = nil

	// Close all active connections.
	for _, c := range bp.active {
		c.Close()
	}
	bp.active = nil

	bp.mu.Unlock()

	// Wait for background goroutines.
	bp.wg.Wait()

	log.Printf("[pool] Bucket %s — pool closed", bp.bucket.ID)
	return nil
}

// Stats returns current pool statistics.
func (bp *BucketPool) Stats() PoolStats {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return PoolStats{
		BucketID:   bp.bucket.ID,
		Active:     len(bp.active),
		Idle:       len(bp.idle),
		Max:        bp.bucket.MaxConnections,
		WaitQueue:  len(bp.waiters),
	}
}

// PoolStats holds pool statistics.
type PoolStats struct {
	BucketID  string
	Active    int
	Idle      int
	Max       int
	WaitQueue int
}

// ── Internal helpers ─────────────────────────────────────────────────────

// createConn opens a new SQL Server connection for this bucket.
func (bp *BucketPool) createConn(ctx context.Context) (*PooledConn, error) {
	id := bp.nextID.Add(1)

	db, err := sql.Open("sqlserver", bp.bucket.DSN())
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	// We use sql.DB as a single-connection pool (MaxOpenConns=1) so each
	// PooledConn maps 1:1 to a physical SQL Server connection.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0) // We manage lifetime ourselves.

	// Verify the connection is actually reachable.
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	return newPooledConn(id, bp.bucket.ID, db), nil
}

// popIdle removes and returns the most recently used idle connection,
// skipping any that are stale. Returns nil if none available.
func (bp *BucketPool) popIdle() *PooledConn {
	for len(bp.idle) > 0 {
		// Pop from the end (most recently used / LIFO for connection reuse).
		n := len(bp.idle) - 1
		conn := bp.idle[n]
		bp.idle = bp.idle[:n]

		// Skip connections that have been idle too long.
		if bp.bucket.MaxIdleTime > 0 && conn.idleDuration() > bp.bucket.MaxIdleTime {
			conn.Close()
			continue
		}
		return conn
	}
	return nil
}

// removeWaiter removes a specific waiter channel from the wait queue.
func (bp *BucketPool) removeWaiter(ch chan *PooledConn) {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	for i, w := range bp.waiters {
		if w == ch {
			bp.waiters = append(bp.waiters[:i], bp.waiters[i+1:]...)
			metrics.QueueLength.WithLabelValues(bp.bucket.ID).Set(float64(len(bp.waiters)))
			break
		}
	}
}

// resetConnection runs sp_reset_connection to clear session state.
func (bp *BucketPool) resetConnection(conn *PooledConn) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := conn.db.ExecContext(ctx, "EXEC sp_reset_connection")
	return err
}

// updateMetrics refreshes Prometheus gauges for this pool.
func (bp *BucketPool) updateMetrics() {
	metrics.ConnectionsActive.WithLabelValues(bp.bucket.ID).Set(float64(len(bp.active)))
	metrics.ConnectionsIdle.WithLabelValues(bp.bucket.ID).Set(float64(len(bp.idle)))
}

// maintenanceLoop runs periodic eviction and health checks.
func (bp *BucketPool) maintenanceLoop() {
	defer bp.wg.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-bp.stopCh:
			return
		case <-ticker.C:
			bp.evictStale()
			bp.ensureMinIdle()
		}
	}
}

// evictStale removes idle connections that have exceeded max_idle_time.
func (bp *BucketPool) evictStale() {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	if bp.bucket.MaxIdleTime == 0 {
		return
	}

	remaining := make([]*PooledConn, 0, len(bp.idle))
	evicted := 0
	for _, conn := range bp.idle {
		if conn.idleDuration() > bp.bucket.MaxIdleTime {
			conn.Close()
			evicted++
		} else {
			remaining = append(remaining, conn)
		}
	}
	bp.idle = remaining

	if evicted > 0 {
		log.Printf("[pool] Bucket %s — evicted %d stale connections", bp.bucket.ID, evicted)
		bp.updateMetrics()
	}
}

// ensureMinIdle creates new connections to maintain the min_idle threshold.
func (bp *BucketPool) ensureMinIdle() {
	bp.mu.Lock()
	deficit := bp.bucket.MinIdle - len(bp.idle)
	totalCount := len(bp.idle) + len(bp.active)
	headroom := bp.bucket.MaxConnections - totalCount
	if deficit > headroom {
		deficit = headroom
	}
	bp.mu.Unlock()

	if deficit <= 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	created := 0
	for i := 0; i < deficit; i++ {
		conn, err := bp.createConn(ctx)
		if err != nil {
			log.Printf("[pool] Bucket %s — failed to create min_idle connection: %v",
				bp.bucket.ID, err)
			break
		}
		bp.mu.Lock()
		bp.idle = append(bp.idle, conn)
		bp.mu.Unlock()
		created++
	}

	if created > 0 {
		bp.mu.Lock()
		bp.updateMetrics()
		bp.mu.Unlock()
		log.Printf("[pool] Bucket %s — replenished %d idle connections", bp.bucket.ID, created)
	}
}
