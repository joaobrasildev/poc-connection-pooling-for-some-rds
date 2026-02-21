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

// ── Servidor TDS Proxy ────────────────────────────────────────────────
//
// O Server escuta em uma porta TCP (tipicamente 1433) e trata conexões
// TDS recebidas. Cada conexão é tratada em sua própria goroutine.

// Server é o servidor principal do proxy TDS.
type Server struct {
	cfg         *config.Config
	poolMgr     *pool.Manager
	coordinator *coordinator.RedisCoordinator
	dqueue      *queue.DistributedQueue
	router      *Router
	listener    net.Listener

	// activeSessions rastreia o número de sessões ativas.
	activeSessions atomic.Int64

	// done sinaliza quando o servidor parou.
	done chan struct{}

	// wg rastreia goroutines de sessões ativas para shutdown gracioso.
	wg sync.WaitGroup

	// cancel é usado para sinalizar todas as sessões a pararem.
	cancel context.CancelFunc
}

// NewServer cria um novo servidor proxy TDS.
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

// Start começa a escutar por conexões TDS.
func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", s.cfg.Proxy.ListenAddr, s.cfg.Proxy.ListenPort)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	s.listener = listener

	// Criar um contexto cancelável para todas as sessões.
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	log.Printf("[proxy] TDS proxy listening on %s", addr)

	// Aceitar conexões em uma goroutine.
	go s.acceptLoop(ctx)

	return nil
}

// acceptLoop aceita conexões recebidas e inicia handlers de sessão.
func (s *Server) acceptLoop(ctx context.Context) {
	defer close(s.done)

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			// Verificar se o listener foi fechado (shutdown gracioso).
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
			// Pausa breve para evitar busy-loop em erros repetidos.
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

// Stop encerra graciosamente o servidor proxy.
// Para de aceitar novas conexões e aguarda as sessões ativas terminarem.
func (s *Server) Stop(ctx context.Context) error {
	log.Printf("[proxy] Shutting down TDS proxy (active sessions: %d)...",
		s.activeSessions.Load())

	// Parar de aceitar novas conexões.
	if s.listener != nil {
		s.listener.Close()
	}

	// Cancelar todas as sessões.
	if s.cancel != nil {
		s.cancel()
	}

	// Aguardar todas as sessões terminarem com um timeout.
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

// ActiveSessions retorna o número de sessões atualmente ativas.
func (s *Server) ActiveSessions() int64 {
	return s.activeSessions.Load()
}

// isListenerClosed verifica se um erro indica que o listener foi fechado.
func isListenerClosed(err error) bool {
	if opErr, ok := err.(*net.OpError); ok {
		return opErr.Err.Error() == "use of closed network connection"
	}
	return false
}
