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
// Proxy TDS transparente que encaminha pacotes Pre-Login e TLS handshake
// entre cliente e backend, evitando incompatibilidades de criptografia.
//
// Ciclo de vida:
//   1. Aceitar conexão TCP
//   2. Ler Pre-Login do cliente → rotear para um bucket → conectar ao backend
//   3. Encaminhar Pre-Login ao backend, retransmitir resposta ao cliente
//   4. Retransmitir TLS handshake transparentemente (se criptografia for requerida)
//   5. Ler Login7 (se não criptografado) para logging; caso contrário retransmitir opacamente
//   6. Retransmitir resposta de login do backend ao cliente
//   7. Fase de dados: relay bidirecional de pacotes com detecção de pinning
//   8. Na desconexão: devolver/descartar conexão

var sessionCounter atomic.Uint64

// Session representa uma sessão de conexão de um único cliente através do proxy.
type Session struct {
	id          uint64
	clientConn  net.Conn
	cfg         *config.Config
	poolMgr     *pool.Manager
	coordinator *coordinator.RedisCoordinator
	dqueue      *queue.DistributedQueue
	router      *Router

	// Estado do backend.
	bucketID    string
	backendConn net.Conn
	poolConn    *pool.PooledConn

	// Coordenação distribuída: se adquirimos um slot.
	slotAcquired bool

	// Estado de pinning.
	pinned    bool
	pinReason string

	// Rastreamento do ciclo de vida.
	startedAt time.Time
}

// newSession cria uma nova sessão para uma conexão de cliente recebida.
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

// Handle executa o ciclo de vida completo da sessão TDS.
func (s *Session) Handle(ctx context.Context) {
	defer s.cleanup()

	clientAddr := s.clientConn.RemoteAddr().String()
	log.Printf("[session:%d] New connection from %s", s.id, clientAddr)

	if s.cfg.Proxy.SessionTimeout > 0 {
		deadline := time.Now().Add(s.cfg.Proxy.SessionTimeout)
		_ = s.clientConn.SetDeadline(deadline)
	}

	// ── Passo 1: Ler Pre-Login do cliente ───────────────────────────
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

	// ── Passo 2: Rotear para um bucket ──────────────────────────────
	// Pre-Login não tem info de user/database; escolher o primeiro bucket.
	// Futuro: rotear por IP do cliente, SNI ou token SSPI.
	target := s.pickBucket()
	if target == nil {
		log.Printf("[session:%d] No buckets configured", s.id)
		return
	}
	s.bucketID = target.ID

	// ── Passo 3: Adquirir slot distribuído (Fase 3 + Fila da Fase 4) ────
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
		// Fallback: usar coordinator diretamente se não houver dqueue (não deveria acontecer no fluxo normal)
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

	// ── Passo 5: Encaminhar Pre-Login ao backend ────────────────────
	if err := tds.WritePackets(s.backendConn, preLoginPackets); err != nil {
		log.Printf("[session:%d] Failed to forward Pre-Login: %v", s.id, err)
		return
	}

	// ── Passo 6: Ler resposta Pre-Login do backend, encaminhar ao cliente ──
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

	// ── Passo 7: Relay TCP bidirecional ─────────────────────────────
	// Após o Pre-Login, o TLS handshake + Login7 + fase de dados acontecem
	// no mesmo stream TCP. Em vez de tentar parsear pacotes TDS
	// durante TLS (que encapsula tudo em registros criptografados opacos),
	// fazemos um splice TCP bruto. Isso trata transparentemente:
	//   - TLS handshake (ClientHello, ServerHello, etc.)
	//   - Login7 criptografado com TLS
	//   - Resposta de login
	//   - Fase de dados (queries, resultados)
	//
	// Para detecção de pinning (Fase 3+), adicionaremos parsing TDS-aware
	// apenas no modo ENCRYPT_NOT_SUP onde os dados não são criptografados.
	log.Printf("[session:%d] Starting bidirectional TCP relay", s.id)
	metrics.ConnectionsActive.WithLabelValues(target.ID).Add(1)
	defer metrics.ConnectionsActive.WithLabelValues(target.ID).Add(-1)

	s.tcpRelay()
}

// pickBucket seleciona um bucket backend para esta sessão.
// Como o Pre-Login não tem info de user/database, pegamos o primeiro bucket
// ou podemos usar round-robin. Para a POC usamos bucket[0].
// Quando roteamento Login7 for necessário pré-conexão, podemos adicionar
// roteamento em duas fases (conectar a um backend temporário, ler Login7, depois re-rotear).
func (s *Session) pickBucket() *bucket.Bucket {
	if len(s.cfg.Buckets) == 0 {
		return nil
	}
	// Simples: usar o primeiro bucket. O Router ainda está disponível para
	// roteamento baseado em Login7 em fases futuras.
	b := &s.cfg.Buckets[0]
	log.Printf("[session:%d] Picked bucket %s (default)", s.id, b.ID)
	return b
}

// tcpRelay realiza cópia bruta bidirecional de bytes TCP entre cliente
// e backend. Isso trata TLS, Login7 e a fase de dados transparentemente.
func (s *Session) tcpRelay() {
	done := make(chan struct{})

	// Cliente → Backend
	go func() {
		_, _ = io.Copy(s.backendConn, s.clientConn)
		// Sinalizar a outra direção fechando o lado de escrita.
		if tc, ok := s.backendConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	// Backend → Cliente
	go func() {
		_, _ = io.Copy(s.clientConn, s.backendConn)
		if tc, ok := s.clientConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	// Aguardar pelo menos uma direção terminar.
	<-done
	log.Printf("[session:%d] TCP relay ended", s.id)
}

// applyPinResult atualiza o estado de pinning da sessão.
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

// sendError envia uma resposta de erro TDS ao cliente.
func (s *Session) sendError(errorPacket []byte) {
	if _, err := s.clientConn.Write(errorPacket); err != nil {
		log.Printf("[session:%d] Failed to send error to client: %v", s.id, err)
	}
}

// cleanup fecha todas as conexões e libera recursos do pool.
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

	// Liberar slot distribuído (Fase 3 + Fase 4).
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

// isConnectionClosed verifica se um erro indica uma conexão fechada.
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
