package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/joao-brasil/poc-connection-pooling/internal/config"
	"github.com/joao-brasil/poc-connection-pooling/internal/coordinator"
	"github.com/joao-brasil/poc-connection-pooling/internal/metrics"
	"github.com/joao-brasil/poc-connection-pooling/internal/pool"
	"github.com/joao-brasil/poc-connection-pooling/internal/queue"
	"github.com/joao-brasil/poc-connection-pooling/internal/tds"
	"github.com/joao-brasil/poc-connection-pooling/pkg/bucket"
)

// ── Session Handler ─────────────────────────────────────────────────────
//
// Transparent TDS proxy that forwards Pre-Login and TLS handshake
// packets between client and backend, avoiding encryption mismatches.
//
// Lifecycle:
//   1. Accept TCP connection
//   2. Read client Pre-Login → route to a bucket → connect to backend
//   3. Forward Pre-Login to backend, relay response back to client
//   4. Relay TLS handshake transparently (if encryption is required)
//   5. Read Login7 (if unencrypted) for logging; otherwise relay opaquely
//   6. Relay login response from backend to client
//   7. Data phase: bidirectional packet relay with pinning detection
//   8. On disconnect: release/discard connection

var sessionCounter atomic.Uint64

// Session represents a single client connection session through the proxy.
type Session struct {
	id          uint64
	clientConn  net.Conn
	cfg         *config.Config
	poolMgr     *pool.Manager
	coordinator *coordinator.RedisCoordinator
	dqueue      *queue.DistributedQueue
	router      *Router

	// Backend state.
	bucketID    string
	backendConn net.Conn
	poolConn    *pool.PooledConn

	// Distributed coordination: whether we acquired a slot.
	slotAcquired bool

	// Pinning state.
	pinned    bool
	pinReason string

	// Lifecycle tracking.
	startedAt time.Time
}

// newSession creates a new session for an incoming client connection.
func newSession(clientConn net.Conn, cfg *config.Config, poolMgr *pool.Manager, rc *coordinator.RedisCoordinator, dq *queue.DistributedQueue, router *Router) *Session {
	return &Session{
		id:          sessionCounter.Add(1),
		clientConn:  clientConn,
		cfg:         cfg,
		poolMgr:     poolMgr,
		coordinator: rc,
		dqueue:      dq,
		router:      router,
		startedAt:   time.Now(),
	}
}

// Handle runs the full TDS session lifecycle.
func (s *Session) Handle(ctx context.Context) {
	defer s.cleanup()

	clientAddr := s.clientConn.RemoteAddr().String()
	log.Printf("[session:%d] New connection from %s", s.id, clientAddr)

	if s.cfg.Proxy.SessionTimeout > 0 {
		deadline := time.Now().Add(s.cfg.Proxy.SessionTimeout)
		_ = s.clientConn.SetDeadline(deadline)
	}

	// ── Step 1: Read client Pre-Login ───────────────────────────────
	preLoginType, preLoginPayload, preLoginPackets, err := tds.ReadMessage(s.clientConn)
	if err != nil {
		log.Printf("[session:%d] Pre-Login read failed: %v", s.id, err)
		return
	}
	if preLoginType != tds.PacketPreLogin {
		log.Printf("[session:%d] Expected PRELOGIN, got %s", s.id, preLoginType)
		return
	}
	clientPL, err := tds.ParsePreLogin(preLoginPayload)
	if err != nil {
		log.Printf("[session:%d] Pre-Login parse failed: %v", s.id, err)
		return
	}
	log.Printf("[session:%d] Pre-Login received, encryption=0x%02X", s.id, clientPL.Encryption())

	// ── Step 2: Route to a bucket ───────────────────────────────────
	// Pre-Login has no user/database info; pick the first bucket now.
	// Future: route by client IP, SNI, or SSPI token.
	target := s.pickBucket()
	if target == nil {
		log.Printf("[session:%d] No buckets configured", s.id)
		return
	}
	s.bucketID = target.ID

	// ── Step 3: Acquire distributed slot (Phase 3 + Phase 4 queue) ─────
	if s.dqueue != nil {
		if err := s.dqueue.Acquire(ctx, target.ID); err != nil {
			log.Printf("[session:%d] Queue acquire failed for bucket %s: %v", s.id, target.ID, err)
			if queue.IsQueueFull(err) {
				s.sendError(tds.ErrQueueFull(target.ID))
				metrics.ConnectionErrors.WithLabelValues(target.ID, "queue_full").Inc()
			} else if queue.IsQueueTimeout(err) {
				s.sendError(tds.ErrQueueTimeout(target.ID))
				metrics.ConnectionErrors.WithLabelValues(target.ID, "queue_timeout").Inc()
			} else {
				s.sendError(tds.ErrBackendUnavailable(target.ID))
				metrics.ConnectionErrors.WithLabelValues(target.ID, "coordinator_acquire_failed").Inc()
			}
			return
		}
		s.slotAcquired = true
		log.Printf("[session:%d] Distributed slot acquired for bucket %s", s.id, target.ID)
	} else if s.coordinator != nil {
		// Fallback: use coordinator directly if no dqueue (shouldn't happen in normal flow)
		if err := s.coordinator.Acquire(ctx, target.ID); err != nil {
			log.Printf("[session:%d] Distributed acquire failed for bucket %s: %v", s.id, target.ID, err)
			s.sendError(tds.ErrBackendUnavailable(target.ID))
			metrics.ConnectionErrors.WithLabelValues(target.ID, "coordinator_acquire_failed").Inc()
			return
		}
		s.slotAcquired = true
		log.Printf("[session:%d] Distributed slot acquired for bucket %s", s.id, target.ID)
	}

	backendAddr := net.JoinHostPort(target.Host, fmt.Sprintf("%d", target.Port))
	dialTimeout := target.ConnectionTimeout
	if dialTimeout == 0 {
		dialTimeout = 30 * time.Second
	}
	backendConn, err := net.DialTimeout("tcp", backendAddr, dialTimeout)
	if err != nil {
		log.Printf("[session:%d] Backend dial failed (%s): %v", s.id, backendAddr, err)
		s.sendError(tds.ErrBackendUnavailable(target.ID))
		metrics.ConnectionErrors.WithLabelValues(target.ID, "dial_failed").Inc()
		return
	}
	s.backendConn = backendConn
	log.Printf("[session:%d] Connected to backend %s (bucket %s)", s.id, backendAddr, target.ID)

	// ── Step 5: Forward Pre-Login to backend ────────────────────────
	if err := tds.WritePackets(s.backendConn, preLoginPackets); err != nil {
		log.Printf("[session:%d] Failed to forward Pre-Login: %v", s.id, err)
		return
	}

	// ── Step 6: Read backend Pre-Login response, forward to client ──
	_, _, respPackets, err := tds.ReadMessage(s.backendConn)
	if err != nil {
		log.Printf("[session:%d] Backend Pre-Login response failed: %v", s.id, err)
		return
	}
	if err := tds.WritePackets(s.clientConn, respPackets); err != nil {
		log.Printf("[session:%d] Failed to relay Pre-Login response: %v", s.id, err)
		return
	}
	log.Printf("[session:%d] Pre-Login handshake relayed", s.id)

	// ── Step 7: Bidirectional TCP relay ─────────────────────────────
	// After Pre-Login, the TLS handshake + Login7 + data phase all happen
	// over the same TCP stream. Instead of trying to parse TDS packets
	// during TLS (which wraps everything in opaque encrypted records),
	// we do a raw TCP splice. This transparently handles:
	//   - TLS handshake (ClientHello, ServerHello, etc.)
	//   - TLS-encrypted Login7
	//   - Login response
	//   - Data phase (queries, results)
	//
	// For pinning detection (Phase 3+), we will add TDS-aware parsing
	// only in ENCRYPT_NOT_SUP mode where data is unencrypted.
	log.Printf("[session:%d] Starting bidirectional TCP relay", s.id)
	metrics.ConnectionsActive.WithLabelValues(target.ID).Add(1)
	defer metrics.ConnectionsActive.WithLabelValues(target.ID).Add(-1)

	s.tcpRelay()
}

// pickBucket selects a backend bucket for this session.
// Since Pre-Login has no user/database info, we pick the first bucket
// or could use round-robin. For the POC we just use bucket[0].
// Once Login7 routing is needed pre-connection, we can add two-phase
// routing (connect to a temp backend, read Login7, then re-route).
func (s *Session) pickBucket() *bucket.Bucket {
	if len(s.cfg.Buckets) == 0 {
		return nil
	}
	// Simple: use the first bucket. The Router is still available for
	// Login7-based routing in future phases.
	b := &s.cfg.Buckets[0]
	log.Printf("[session:%d] Picked bucket %s (default)", s.id, b.ID)
	return b
}

// tcpRelay performs raw bidirectional TCP byte copying between client
// and backend. This handles TLS, Login7, and the data phase transparently.
func (s *Session) tcpRelay() {
	done := make(chan struct{})

	// Client → Backend
	go func() {
		_, _ = io.Copy(s.backendConn, s.clientConn)
		// Signal the other direction by closing the write side.
		if tc, ok := s.backendConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	// Backend → Client
	go func() {
		_, _ = io.Copy(s.clientConn, s.backendConn)
		if tc, ok := s.clientConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	// Wait for at least one direction to finish.
	<-done
	log.Printf("[session:%d] TCP relay ended", s.id)
}

// applyPinResult updates the session's pinning state.
func (s *Session) applyPinResult(result tds.PinResult) {
	switch result.Action {
	case tds.PinActionPin:
		if !s.pinned {
			s.pinned = true
			s.pinReason = result.Reason
			log.Printf("[session:%d] Connection pinned: %s", s.id, result.Reason)
			metrics.ConnectionsPinned.WithLabelValues(s.bucketID, result.Reason).Inc()
		}
	case tds.PinActionUnpin:
		if s.pinned {
			s.pinned = false
			log.Printf("[session:%d] Connection unpinned (was: %s)", s.id, s.pinReason)
			metrics.ConnectionsPinned.WithLabelValues(s.bucketID, s.pinReason).Dec()
			s.pinReason = ""
		}
	}
}

// sendError sends a TDS error response to the client.
func (s *Session) sendError(errorPacket []byte) {
	if _, err := s.clientConn.Write(errorPacket); err != nil {
		log.Printf("[session:%d] Failed to send error to client: %v", s.id, err)
	}
}

// cleanup closes all connections and releases pool resources.
func (s *Session) cleanup() {
	duration := time.Since(s.startedAt)
	log.Printf("[session:%d] Session ended after %v (bucket=%s, pinned=%v)",
		s.id, duration, s.bucketID, s.pinned)

	if s.clientConn != nil {
		s.clientConn.Close()
	}
	if s.backendConn != nil {
		s.backendConn.Close()
	}
	if s.poolConn != nil {
		if s.pinned {
			s.poolMgr.Discard(s.poolConn)
		} else {
			s.poolMgr.Release(s.poolConn)
		}
	}

	// Release distributed slot (Phase 3 + Phase 4).
	if s.slotAcquired {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if s.dqueue != nil {
			if err := s.dqueue.Release(ctx, s.bucketID); err != nil {
				log.Printf("[session:%d] Distributed release (dqueue) failed for bucket %s: %v",
					s.id, s.bucketID, err)
			}
		} else if s.coordinator != nil {
			if err := s.coordinator.Release(ctx, s.bucketID); err != nil {
				log.Printf("[session:%d] Distributed release failed for bucket %s: %v",
					s.id, s.bucketID, err)
			}
		}
	}
}

// isConnectionClosed checks if an error indicates a closed connection.
func isConnectionClosed(err error) bool {
	if err == nil {
		return false
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return true
	}
	if netErr, ok := err.(*net.OpError); ok {
		return netErr.Err.Error() == "use of closed network connection"
	}
	return false
}
