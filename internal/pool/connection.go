// Package pool provides the connection pool manager for SQL Server backend connections.
// Each bucket has its own pool with configurable min_idle, max_connections, health checks,
// and sp_reset_connection on release.
package pool

import (
	"database/sql"
	"sync"
	"time"
)

// PinReason describes why a connection is pinned (not returnable to pool).
type PinReason string

const (
	PinNone        PinReason = ""
	PinTransaction PinReason = "transaction"
	PinPrepared    PinReason = "prepared"
	PinBulkLoad    PinReason = "bulk_load"
)

// ConnState represents the lifecycle state of a pooled connection.
type ConnState int

const (
	ConnStateIdle   ConnState = iota // Available in pool
	ConnStateActive                  // Acquired by a client
	ConnStateClosed                  // Removed from pool
)

// PooledConn wraps a *sql.DB connection with metadata for pool management.
// It is the unit managed by the BucketPool.
type PooledConn struct {
	mu sync.Mutex

	// db is the underlying SQL Server connection (via go-mssqldb).
	db *sql.DB

	// id is a unique identifier for this connection within the pool.
	id uint64

	// bucketID identifies which bucket this connection belongs to.
	bucketID string

	// state tracks the current lifecycle state.
	state ConnState

	// pinReason is non-empty when the connection is pinned.
	pinReason PinReason

	// pinnedAt is the time when the connection was pinned.
	pinnedAt time.Time

	// createdAt is the time the connection was established.
	createdAt time.Time

	// lastUsedAt is the last time the connection was acquired or returned.
	lastUsedAt time.Time

	// lastHealthCheck is the last time SELECT 1 was run on this connection.
	lastHealthCheck time.Time

	// useCount tracks how many times this connection was acquired.
	useCount uint64
}

// newPooledConn creates a new PooledConn wrapping a sql.DB.
func newPooledConn(id uint64, bucketID string, db *sql.DB) *PooledConn {
	now := time.Now()
	return &PooledConn{
		db:              db,
		id:              id,
		bucketID:        bucketID,
		state:           ConnStateIdle,
		createdAt:       now,
		lastUsedAt:      now,
		lastHealthCheck: now,
	}
}

// DB returns the underlying *sql.DB.
func (c *PooledConn) DB() *sql.DB {
	return c.db
}

// ID returns the connection's unique identifier.
func (c *PooledConn) ID() uint64 {
	return c.id
}

// BucketID returns the bucket this connection belongs to.
func (c *PooledConn) BucketID() string {
	return c.bucketID
}

// State returns the current connection state.
func (c *PooledConn) State() ConnState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// IsPinned returns true if the connection is pinned.
func (c *PooledConn) IsPinned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pinReason != PinNone
}

// PinReason returns the current pin reason.
func (c *PooledConn) PinReason() PinReason {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pinReason
}

// Pin marks the connection as pinned with the given reason.
func (c *PooledConn) Pin(reason PinReason) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pinReason == PinNone {
		c.pinnedAt = time.Now()
	}
	c.pinReason = reason
}

// Unpin clears the pin reason. Returns the duration the connection was pinned.
func (c *PooledConn) Unpin() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	dur := time.Duration(0)
	if c.pinReason != PinNone {
		dur = time.Since(c.pinnedAt)
	}
	c.pinReason = PinNone
	c.pinnedAt = time.Time{}
	return dur
}

// markAcquired transitions the connection to active state.
func (c *PooledConn) markAcquired() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = ConnStateActive
	c.lastUsedAt = time.Now()
	c.useCount++
}

// markIdle transitions the connection back to idle state.
func (c *PooledConn) markIdle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = ConnStateIdle
	c.lastUsedAt = time.Now()
}

// markClosed transitions the connection to closed state.
func (c *PooledConn) markClosed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = ConnStateClosed
}

// idleDuration returns how long the connection has been idle.
func (c *PooledConn) idleDuration() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Since(c.lastUsedAt)
}

// Close closes the underlying database connection.
func (c *PooledConn) Close() error {
	c.markClosed()
	return c.db.Close()
}
