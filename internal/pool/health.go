package pool

import (
	"context"
	"log"
	"time"
)

// HealthCheck runs SELECT 1 on every idle connection in every pool,
// discarding any that are unhealthy. This is called periodically
// by the maintenance loop.
func (bp *BucketPool) HealthCheck() {
	bp.mu.Lock()
	conns := make([]*PooledConn, len(bp.idle))
	copy(conns, bp.idle)
	bp.mu.Unlock()

	healthy := make([]*PooledConn, 0, len(conns))
	removed := 0

	for _, conn := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := conn.db.PingContext(ctx)
		cancel()

		if err != nil {
			log.Printf("[pool] Bucket %s — health check failed for conn %d: %v",
				bp.bucket.ID, conn.id, err)
			conn.Close()
			removed++
			continue
		}

		conn.mu.Lock()
		conn.lastHealthCheck = time.Now()
		conn.mu.Unlock()

		healthy = append(healthy, conn)
	}

	if removed > 0 {
		bp.mu.Lock()
		// Rebuild idle list with only healthy connections.
		newIdle := make([]*PooledConn, 0, len(bp.idle))
		healthySet := make(map[uint64]bool, len(healthy))
		for _, c := range healthy {
			healthySet[c.id] = true
		}
		for _, c := range bp.idle {
			if healthySet[c.id] {
				newIdle = append(newIdle, c)
			}
		}
		bp.idle = newIdle
		bp.updateMetrics()
		bp.mu.Unlock()

		log.Printf("[pool] Bucket %s — health check: removed %d unhealthy connections",
			bp.bucket.ID, removed)
	}
}
