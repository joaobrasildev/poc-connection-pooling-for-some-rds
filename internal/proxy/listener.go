package proxy

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joao-brasil/poc-connection-pooling/internal/config"
	"github.com/joao-brasil/poc-connection-pooling/internal/coordinator"
	"github.com/joao-brasil/poc-connection-pooling/internal/pool"
	"github.com/joao-brasil/poc-connection-pooling/internal/queue"
)

// ── TDS Proxy Server ────────────────────────────────────────────────────
//
// The Server listens on a TCP port (typically 1433) and handles incoming
// TDS connections. Each connection is handled in its own goroutine.

// Server is the main TDS proxy server.
type Server struct {
	cfg         *config.Config
	poolMgr     *pool.Manager
	coordinator *coordinator.RedisCoordinator
	dqueue      *queue.DistributedQueue
	router      *Router
	listener    net.Listener

	// activeSessions tracks the number of active sessions.
	activeSessions atomic.Int64

	// done signals when the server has stopped.
	done chan struct{}

	// wg tracks active session goroutines for graceful shutdown.
	wg sync.WaitGroup

	// cancel is used to signal all sessions to stop.
	cancel context.CancelFunc
}

// NewServer creates a new TDS proxy server.
func NewServer(cfg *config.Config, poolMgr *pool.Manager, rc *coordinator.RedisCoordinator, dq *queue.DistributedQueue) *Server {
	return &Server{
		cfg:         cfg,
		poolMgr:     poolMgr,
		coordinator: rc,
		dqueue:      dq,
		router:      NewRouter(cfg),
		done:        make(chan struct{}),
	}
}

// Start begins listening for TDS connections.
func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Proxy.ListenAddr, s.cfg.Proxy.ListenPort)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	s.listener = listener

	// Create a cancellable context for all sessions.
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	log.Printf("[proxy] TDS proxy listening on %s", addr)

	// Accept connections in a goroutine.
	go s.acceptLoop(ctx)

	return nil
}

// acceptLoop accepts incoming connections and spawns session handlers.
func (s *Server) acceptLoop(ctx context.Context) {
	defer close(s.done)

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			// Check if the listener was closed (graceful shutdown).
			select {
			case <-ctx.Done():
				return
			default:
			}

			if isListenerClosed(err) {
				log.Printf("[proxy] Listener closed")
				return
			}

			log.Printf("[proxy] Accept error: %v", err)
			// Brief sleep to avoid busy-loop on repeated errors.
			time.Sleep(100 * time.Millisecond)
			continue
		}

		s.activeSessions.Add(1)
		s.wg.Add(1)

		go func() {
			defer s.wg.Done()
			defer s.activeSessions.Add(-1)

			session := newSession(conn, s.cfg, s.poolMgr, s.coordinator, s.dqueue, s.router)
			session.Handle(ctx)
		}()
	}
}

// Stop gracefully shuts down the proxy server.
// It stops accepting new connections and waits for active sessions to finish.
func (s *Server) Stop(ctx context.Context) error {
	log.Printf("[proxy] Shutting down TDS proxy (active sessions: %d)...",
		s.activeSessions.Load())

	// Stop accepting new connections.
	if s.listener != nil {
		s.listener.Close()
	}

	// Cancel all sessions.
	if s.cancel != nil {
		s.cancel()
	}

	// Wait for all sessions to finish with a timeout.
	doneCh := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(doneCh)
	}()

	select {
	case <-doneCh:
		log.Printf("[proxy] All sessions closed gracefully")
	case <-ctx.Done():
		log.Printf("[proxy] Shutdown timeout — some sessions may have been interrupted")
	}

	return nil
}

// ActiveSessions returns the number of currently active sessions.
func (s *Server) ActiveSessions() int64 {
	return s.activeSessions.Load()
}

// isListenerClosed checks if an error indicates the listener was closed.
func isListenerClosed(err error) bool {
	if opErr, ok := err.(*net.OpError); ok {
		return opErr.Err.Error() == "use of closed network connection"
	}
	return false
}
