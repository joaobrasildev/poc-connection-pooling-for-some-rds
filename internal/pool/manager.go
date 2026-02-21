package pool

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/joao-brasil/poc-connection-pooling/internal/config"
	"github.com/joao-brasil/poc-connection-pooling/pkg/bucket"
)

// Manager gerencia connection pools para todos os buckets configurados.
// É o ponto de entrada principal para a Fase 1 — pooling de instância única.
// Na Fase 3, o coordinator (Redis) encapsula o Manager para limites distribuídos.
type Manager struct {
	mu    sync.RWMutex
	pools map[string]*BucketPool // keyed by bucket ID
	cfg   *config.Config
}

// NewManager cria um Manager e inicializa um BucketPool para cada bucket.
func NewManager(ctx context.Context, cfg *config.Config) (*Manager, error) {
	m := &Manager{
		pools: make(map[string]*BucketPool, len(cfg.Buckets)),
		cfg:   cfg,
	}

	for i := range cfg.Buckets {
		b := &cfg.Buckets[i]
		pool, err := NewBucketPool(ctx, b)
		if err != nil {
			// Fechar quaisquer pools já criados antes de retornar.
			m.Close()
			return nil, fmt.Errorf("initializing pool for bucket %s: %w", b.ID, err)
		}
		m.pools[b.ID] = pool
	}

	log.Printf("[pool] Manager initialized: %d bucket pools", len(m.pools))
	return m, nil
}

// Acquire obtém uma conexão do pool para o bucket especificado.
func (m *Manager) Acquire(ctx context.Context, bucketID string) (*PooledConn, error) {
	m.mu.RLock()
	pool, ok := m.pools[bucketID]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("unknown bucket: %s", bucketID)
	}

	return pool.Acquire(ctx)
}

// AcquireForBucket obtém uma conexão do pool para a configuração de bucket especificada.
func (m *Manager) AcquireForBucket(ctx context.Context, b *bucket.Bucket) (*PooledConn, error) {
	return m.Acquire(ctx, b.ID)
}

// Release devolve uma conexão de volta ao pool do seu bucket.
func (m *Manager) Release(conn *PooledConn) {
	if conn == nil {
		return
	}

	m.mu.RLock()
	pool, ok := m.pools[conn.BucketID()]
	m.mu.RUnlock()

	if !ok {
		log.Printf("[pool] WARNING: releasing connection for unknown bucket %s, closing", conn.BucketID())
		conn.Close()
		return
	}

	pool.Release(conn)
}

// Discard remove uma conexão permanentemente do pool do seu bucket.
func (m *Manager) Discard(conn *PooledConn) {
	if conn == nil {
		return
	}

	m.mu.RLock()
	pool, ok := m.pools[conn.BucketID()]
	m.mu.RUnlock()

	if !ok {
		conn.Close()
		return
	}

	pool.Discard(conn)
}

// Stats retorna estatísticas do pool para todos os buckets.
func (m *Manager) Stats() []PoolStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := make([]PoolStats, 0, len(m.pools))
	for _, p := range m.pools {
		stats = append(stats, p.Stats())
	}
	return stats
}

// Pool retorna o BucketPool para um dado ID de bucket.
func (m *Manager) Pool(bucketID string) (*BucketPool, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.pools[bucketID]
	return p, ok
}

// Close encerra todos os bucket pools.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var firstErr error
	for id, p := range m.pools {
		if err := p.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("closing pool %s: %w", id, err)
		}
	}
	m.pools = nil

	log.Println("[pool] Manager closed")
	return firstErr
}
