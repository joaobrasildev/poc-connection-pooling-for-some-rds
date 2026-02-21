// Package pool fornece o gerenciador de connection pool para conexões backend do SQL Server.
// Cada bucket tem seu próprio pool com min_idle, max_connections, health checks
// e sp_reset_connection no release configuráveis.
package pool

import (
	"database/sql"
	"sync"
	"time"
)

// PinReason descreve por que uma conexão está pinada (não retornável ao pool).
type PinReason string

const (
	PinNone        PinReason = ""
	PinTransaction PinReason = "transaction"
	PinPrepared    PinReason = "prepared"
	PinBulkLoad    PinReason = "bulk_load"
)

// ConnState representa o estado do ciclo de vida de uma conexão no pool.
type ConnState int

const (
	ConnStateIdle   ConnState = iota // Disponível no pool
	ConnStateActive                  // Adquirida por um cliente
	ConnStateClosed                  // Removida do pool
)

// PooledConn encapsula uma conexão *sql.DB com metadados para gerenciamento de pool.
// É a unidade gerenciada pelo BucketPool.
type PooledConn struct {
	mu sync.Mutex

	// db é a conexão SQL Server subjacente (via go-mssqldb).
	db *sql.DB

	// id é um identificador único para esta conexão dentro do pool.
	id uint64

	// bucketID identifica a qual bucket esta conexão pertence.
	bucketID string

	// state rastreia o estado atual do ciclo de vida.
	state ConnState

	// pinReason é não-vazio quando a conexão está pinada.
	pinReason PinReason

	// pinnedAt é o momento em que a conexão foi pinada.
	pinnedAt time.Time

	// createdAt é o momento em que a conexão foi estabelecida.
	createdAt time.Time

	// lastUsedAt é a última vez que a conexão foi adquirida ou devolvida.
	lastUsedAt time.Time

	// lastHealthCheck é a última vez que SELECT 1 foi executado nesta conexão.
	lastHealthCheck time.Time

	// useCount rastreia quantas vezes esta conexão foi adquirida.
	useCount uint64
}

// newPooledConn cria uma nova PooledConn encapsulando um sql.DB.
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

// DB retorna o *sql.DB subjacente.
func (c *PooledConn) DB() *sql.DB {
	return c.db
}

// ID retorna o identificador único da conexão.
func (c *PooledConn) ID() uint64 {
	return c.id
}

// BucketID retorna o bucket ao qual esta conexão pertence.
func (c *PooledConn) BucketID() string {
	return c.bucketID
}

// State retorna o estado atual da conexão.
func (c *PooledConn) State() ConnState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// IsPinned retorna true se a conexão estiver pinada.
func (c *PooledConn) IsPinned() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pinReason != PinNone
}

// PinReason retorna o motivo atual do pin.
func (c *PooledConn) PinReason() PinReason {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pinReason
}

// Pin marca a conexão como pinada com o motivo especificado.
func (c *PooledConn) Pin(reason PinReason) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pinReason == PinNone {
		c.pinnedAt = time.Now()
	}
	c.pinReason = reason
}

// Unpin limpa o motivo de pin. Retorna a duração em que a conexão ficou pinada.
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

// markAcquired transiciona a conexão para o estado ativo.
func (c *PooledConn) markAcquired() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = ConnStateActive
	c.lastUsedAt = time.Now()
	c.useCount++
}

// markIdle transiciona a conexão de volta para o estado idle.
func (c *PooledConn) markIdle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = ConnStateIdle
	c.lastUsedAt = time.Now()
}

// markClosed transiciona a conexão para o estado fechado.
func (c *PooledConn) markClosed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = ConnStateClosed
}

// idleDuration retorna há quanto tempo a conexão está idle.
func (c *PooledConn) idleDuration() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Since(c.lastUsedAt)
}

// Close fecha a conexão de banco de dados subjacente.
func (c *PooledConn) Close() error {
	c.markClosed()
	return c.db.Close()
}
