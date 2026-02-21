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

// BucketPool gerencia um pool de conexões SQL Server para um único bucket.
// Fornece semântica de acquire/release com limites configuráveis, um pool aquecido
// de conexões idle, evição de conexões stale, e health checking.
type BucketPool struct {
	mu sync.Mutex

	bucket *bucket.Bucket

	// idle mantém conexões disponíveis para reuso, a mais recentemente usada primeiro.
	idle []*PooledConn

	// active rastreia conexões atualmente em uso (indexadas pelo ID da conexão).
	active map[uint64]*PooledConn

	// nextID é um contador atômico para atribuir IDs únicos de conexão.
	nextID atomic.Uint64

	// closed indica se o pool foi encerrado.
	closed bool

	// waiters é uma fila baseada em channel para chamadores aguardando uma conexão.
	// Cada waiter envia um channel que receberá a conexão alocada.
	waiters []chan *PooledConn

	// notify é usado para sinalizar que uma conexão foi devolvida ao pool.
	notify chan struct{}

	// stopCh sinaliza goroutines em segundo plano para parar.
	stopCh chan struct{}

	// wg rastreia goroutines em segundo plano.
	wg sync.WaitGroup
}

// NewBucketPool cria um novo pool para o bucket especificado e abre eagerly min_idle conexões.
func NewBucketPool(ctx context.Context, b *bucket.Bucket) (*BucketPool, error) {
	bp := &BucketPool{
		bucket:  b,
		idle:    make([]*PooledConn, 0, b.MaxConnections),
		active:  make(map[uint64]*PooledConn),
		notify:  make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
	}

	// Criar eagerly min_idle conexões (pool aquecido).
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

	// Iniciar manutenção em segundo plano.
	bp.wg.Add(1)
	go bp.maintenanceLoop()

	return bp, nil
}

// Acquire obtém uma conexão do pool. Se nenhuma conexão estiver disponível
// e o pool estiver na capacidade máxima, o chamador bloqueia até que uma
// conexão seja liberada ou o context expire.
func (bp *BucketPool) Acquire(ctx context.Context) (*PooledConn, error) {
	start := time.Now()

	bp.mu.Lock()
	if bp.closed {
		bp.mu.Unlock()
		return nil, fmt.Errorf("pool closed for bucket %s", bp.bucket.ID)
	}

	// Tentar obter uma conexão idle.
	if conn := bp.popIdle(); conn != nil {
		bp.active[conn.id] = conn
		conn.markAcquired()
		bp.updateMetrics()
		bp.mu.Unlock()
		metrics.ConnectionsTotal.WithLabelValues(bp.bucket.ID, "acquired").Inc()
		return conn, nil
	}

	// Se abaixo do máximo, criar uma nova conexão.
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

	// Pool está cheio — entrar na fila de espera.
	waiterCh := make(chan *PooledConn, 1)
	bp.waiters = append(bp.waiters, waiterCh)
	metrics.QueueLength.WithLabelValues(bp.bucket.ID).Set(float64(len(bp.waiters)))
	bp.mu.Unlock()

	log.Printf("[pool] Bucket %s — connection queue entered, position=%d",
		bp.bucket.ID, len(bp.waiters))

	// Aguardar uma conexão, cancelamento de context, ou timeout da fila.
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

// Release devolve uma conexão ao pool. Executa sp_reset_connection
// para limpar o estado da sessão antes de torná-la disponível para reuso.
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

	// Resetar estado da sessão para que a conexão seja segura para reuso.
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
	// Entregar a um waiter se houver algum na fila.
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

// Discard remove uma conexão do pool permanentemente (ex: em caso de erro).
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

// Close encerra o pool, fechando todas as conexões e notificando waiters.
func (bp *BucketPool) Close() error {
	bp.mu.Lock()
	if bp.closed {
		bp.mu.Unlock()
		return nil
	}
	bp.closed = true

	// Sinalizar goroutines em segundo plano para parar.
	close(bp.stopCh)

	// Notificar todos os waiters que o pool está fechando.
	for _, w := range bp.waiters {
		close(w)
	}
	bp.waiters = nil

	// Fechar todas as conexões idle.
	for _, c := range bp.idle {
		c.Close()
	}
	bp.idle = nil

	// Fechar todas as conexões ativas.
	for _, c := range bp.active {
		c.Close()
	}
	bp.active = nil

	bp.mu.Unlock()

	// Aguardar goroutines em segundo plano.
	bp.wg.Wait()

	log.Printf("[pool] Bucket %s — pool closed", bp.bucket.ID)
	return nil
}

// Stats retorna as estatísticas atuais do pool.
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

// PoolStats contém as estatísticas do pool.
type PoolStats struct {
	BucketID  string
	Active    int
	Idle      int
	Max       int
	WaitQueue int
}

// ── Auxiliares internos ─────────────────────────────────────────────────────

// createConn abre uma nova conexão SQL Server para este bucket.
func (bp *BucketPool) createConn(ctx context.Context) (*PooledConn, error) {
	id := bp.nextID.Add(1)

	db, err := sql.Open("sqlserver", bp.bucket.DSN())
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	// Usamos sql.DB como pool de conexão única (MaxOpenConns=1) para que cada
	// PooledConn mapeie 1:1 para uma conexão física do SQL Server.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0) // Gerenciamos o tempo de vida nós mesmos.

	// Verificar se a conexão é realmente alcançável.
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	return newPooledConn(id, bp.bucket.ID, db), nil
}

// popIdle remove e retorna a conexão idle mais recentemente usada,
// pulando qualquer uma que esteja stale. Retorna nil se nenhuma estiver disponível.
func (bp *BucketPool) popIdle() *PooledConn {
	for len(bp.idle) > 0 {
		// Remover do final (mais recentemente usada / LIFO para reuso de conexão).
		n := len(bp.idle) - 1
		conn := bp.idle[n]
		bp.idle = bp.idle[:n]

		// Pular conexões que ficaram idle por tempo demais.
		if bp.bucket.MaxIdleTime > 0 && conn.idleDuration() > bp.bucket.MaxIdleTime {
			conn.Close()
			continue
		}
		return conn
	}
	return nil
}

// removeWaiter remove um channel de waiter específico da fila de espera.
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

// resetConnection executa sp_reset_connection para limpar o estado da sessão.
func (bp *BucketPool) resetConnection(conn *PooledConn) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := conn.db.ExecContext(ctx, "EXEC sp_reset_connection")
	return err
}

// updateMetrics atualiza os gauges do Prometheus para este pool.
func (bp *BucketPool) updateMetrics() {
	metrics.ConnectionsActive.WithLabelValues(bp.bucket.ID).Set(float64(len(bp.active)))
	metrics.ConnectionsIdle.WithLabelValues(bp.bucket.ID).Set(float64(len(bp.idle)))
}

// maintenanceLoop executa evição periódica e health checks.
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

// evictStale remove conexões idle que excederam o max_idle_time.
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

// ensureMinIdle cria novas conexões para manter o limiar de min_idle.
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
