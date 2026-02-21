// Package queue fornece mecanismos de fila distribuída para coordenação
// cross-instance de espera por conexões. Encapsula as notificações Pub/Sub
// do coordinator e o semáforo distribuído para fornecer uma interface
// unificada de espera para o connection pool.
//
// Adições da Fase 4: circuit breaker (tamanho máximo da fila), métricas
// por bucket e rejeição graciosa com suporte a erros TDS.
package queue

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/joao-brasil/poc-connection-pooling/internal/coordinator"
	"github.com/joao-brasil/poc-connection-pooling/internal/metrics"
)

// DistributedQueue gerencia filas de espera distribuídas para todos os buckets.
// Quando um pool local está na capacidade global, os chamadores esperam no
// semáforo distribuído. Quando qualquer instância de proxy libera uma conexão,
// todas as instâncias em espera são notificadas via Pub/Sub para que uma
// delas possa adquirir o slot.
type DistributedQueue struct {
	coordinator *coordinator.RedisCoordinator
	semaphore   *coordinator.Semaphore

	// rastreamento de profundidade da fila por bucket
	mu     sync.Mutex
	depths map[string]int

	timeout      time.Duration // tempo máximo de espera por requisição
	maxQueueSize int           // profundidade máxima da fila por bucket (0 = ilimitado)
}

// NewDistributedQueue cria uma nova fila distribuída apoiada pelo coordinator.
func NewDistributedQueue(rc *coordinator.RedisCoordinator, timeout time.Duration, maxQueueSize int) *DistributedQueue {
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &DistributedQueue{
		coordinator:  rc,
		semaphore:    coordinator.NewSemaphore(rc),
		depths:       make(map[string]int),
		timeout:      timeout,
		maxQueueSize: maxQueueSize,
	}
}

// Acquire tenta obter um slot distribuído para o bucket fornecido.
// Primeiro tenta uma aquisição imediata. Se falhar (bucket na capacidade),
// verifica o circuit breaker (tamanho máximo da fila) e entra na fila
// de espera distribuída usando o semáforo.
//
// Retorna nil se um slot foi adquirido, ou um erro em timeout/cancelamento/rejeição.
// O tipo de erro pode ser verificado para determinar o erro TDS apropriado a enviar:
//   - ErrQueueFull: circuit breaker disparado (fila na capacidade máxima)
//   - ErrQueueTimeout: esperou mas esgotou o timeout
//   - context.Canceled / context.DeadlineExceeded: cliente desconectou
func (dq *DistributedQueue) Acquire(ctx context.Context, bucketID string) error {
	// Caminho rápido: tentar aquisição não-bloqueante.
	if err := dq.semaphore.TryAcquire(ctx, bucketID); err == nil {
		metrics.ConnectionsTotal.WithLabelValues(bucketID, "acquired").Inc()
		return nil
	}

	// Circuit breaker: rejeitar imediatamente se a fila já está na profundidade máxima.
	if dq.maxQueueSize > 0 {
		currentDepth := dq.getDepth(bucketID)
		if currentDepth >= dq.maxQueueSize {
			metrics.ConnectionsTotal.WithLabelValues(bucketID, "rejected_queue_full").Inc()
			log.Printf("[dqueue] Circuit breaker: rejecting request for bucket %s (queue depth=%d, max=%d)",
				bucketID, currentDepth, dq.maxQueueSize)
			return &QueueError{
				BucketID: bucketID,
				Kind:     QueueErrorFull,
				Depth:    currentDepth,
				MaxSize:  dq.maxQueueSize,
			}
		}
	}

	// Caminho lento: entrar na fila de espera distribuída.
	dq.incrementDepth(bucketID)
	defer dq.decrementDepth(bucketID)

	log.Printf("[dqueue] Entering distributed wait for bucket %s (depth=%d, timeout=%s)",
		bucketID, dq.getDepth(bucketID), dq.timeout)

	start := time.Now()
	err := dq.semaphore.Wait(ctx, bucketID, dq.timeout)
	dur := time.Since(start)

	if err != nil {
		// Classificar o erro.
		if ctx.Err() != nil {
			metrics.ConnectionsTotal.WithLabelValues(bucketID, "cancelled").Inc()
			log.Printf("[dqueue] Wait cancelled for bucket %s after %v: %v", bucketID, dur, err)
			return ctx.Err()
		}
		metrics.ConnectionsTotal.WithLabelValues(bucketID, "timeout").Inc()
		log.Printf("[dqueue] Wait timed out for bucket %s after %v: %v", bucketID, dur, err)
		return &QueueError{
			BucketID: bucketID,
			Kind:     QueueErrorTimeout,
			WaitTime: dur,
			Timeout:  dq.timeout,
		}
	}

	metrics.ConnectionsTotal.WithLabelValues(bucketID, "acquired_after_wait").Inc()
	log.Printf("[dqueue] Acquired slot for bucket %s after %v wait", bucketID, dur)
	return nil
}

// Release notifica a fila distribuída que uma conexão foi liberada.
// Isso é tratado internamente pelo script Lua do coordinator (PUBLISH).
// Chamar este método explicitamente garante que o release do coordinator seja invocado.
func (dq *DistributedQueue) Release(ctx context.Context, bucketID string) error {
	return dq.coordinator.Release(ctx, bucketID)
}

// Depth retorna a profundidade atual da fila de espera distribuída para um bucket.
func (dq *DistributedQueue) Depth(bucketID string) int {
	return dq.getDepth(bucketID)
}

// ── Tipos de Erro de Fila ─────────────────────────────────────────────

// QueueErrorKind classifica o tipo de erro de fila.
type QueueErrorKind int

const (
	// QueueErrorTimeout significa que a requisição esperou o período completo de timeout.
	QueueErrorTimeout QueueErrorKind = iota
	// QueueErrorFull significa que a fila está na capacidade máxima (circuit breaker).
	QueueErrorFull
)

// QueueError fornece informações estruturadas de erro para falhas de fila.
type QueueError struct {
	BucketID string
	Kind     QueueErrorKind
	Depth    int           // profundidade atual da fila (para QueueErrorFull)
	MaxSize  int           // tamanho máximo da fila (para QueueErrorFull)
	WaitTime time.Duration // quanto tempo a requisição esperou (para QueueErrorTimeout)
	Timeout  time.Duration // timeout configurado (para QueueErrorTimeout)
}

func (e *QueueError) Error() string {
	switch e.Kind {
	case QueueErrorFull:
		return fmt.Sprintf("queue full for bucket %s (depth=%d, max=%d)",
			e.BucketID, e.Depth, e.MaxSize)
	case QueueErrorTimeout:
		return fmt.Sprintf("queue timeout for bucket %s (waited=%v, timeout=%v)",
			e.BucketID, e.WaitTime, e.Timeout)
	default:
		return fmt.Sprintf("queue error for bucket %s", e.BucketID)
	}
}

// IsQueueFull verifica se o erro é uma rejeição do circuit breaker.
func IsQueueFull(err error) bool {
	if qe, ok := err.(*QueueError); ok {
		return qe.Kind == QueueErrorFull
	}
	return false
}

// IsQueueTimeout verifica se o erro é um timeout de fila.
func IsQueueTimeout(err error) bool {
	if qe, ok := err.(*QueueError); ok {
		return qe.Kind == QueueErrorTimeout
	}
	return false
}

// ── Helpers internos ─────────────────────────────────────────────────────

func (dq *DistributedQueue) incrementDepth(bucketID string) {
	dq.mu.Lock()
	dq.depths[bucketID]++
	depth := dq.depths[bucketID]
	dq.mu.Unlock()
	metrics.QueueLength.WithLabelValues(bucketID).Set(float64(depth))
}

func (dq *DistributedQueue) decrementDepth(bucketID string) {
	dq.mu.Lock()
	dq.depths[bucketID]--
	if dq.depths[bucketID] < 0 {
		dq.depths[bucketID] = 0
	}
	depth := dq.depths[bucketID]
	dq.mu.Unlock()
	metrics.QueueLength.WithLabelValues(bucketID).Set(float64(depth))
}

func (dq *DistributedQueue) getDepth(bucketID string) int {
	dq.mu.Lock()
	defer dq.mu.Unlock()
	return dq.depths[bucketID]
}
