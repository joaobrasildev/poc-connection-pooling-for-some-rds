package coordinator

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/joao-brasil/poc-connection-pooling/internal/metrics"
)

// ── Semáforo Distribuído ─────────────────────────────────────────────
//
// O semáforo fornece um mecanismo de espera distribuído para aquisição
// de conexões. Quando o pool global de um bucket está cheio, os chamadores
// esperam no semáforo até que uma conexão seja liberada por qualquer instância de proxy.
//
// Ele combina:
//   - Redis Pub/Sub para notificações instantâneas cross-instance
//   - Fallback de polling para tratar mensagens Pub/Sub perdidas
//   - Timeout para evitar espera indefinida

// Semaphore fornece espera distribuída por disponibilidade de conexão.
type Semaphore struct {
	coordinator *RedisCoordinator
}

// NewSemaphore cria um novo semáforo distribuído.
func NewSemaphore(rc *RedisCoordinator) *Semaphore {
	return &Semaphore{coordinator: rc}
}

// Wait bloqueia até que um slot de conexão fique disponível para o bucket fornecido,
// então o adquire atomicamente. Retorna um erro se o contexto expirar ou
// o timeout de espera for atingido.
func (s *Semaphore) Wait(ctx context.Context, bucketID string, timeout time.Duration) error {
	// Caminho rápido: tentar aquisição imediata.
	if err := s.coordinator.Acquire(ctx, bucketID); err == nil {
		return nil
	}

	start := time.Now()
	log.Printf("[semaphore] Waiting for connection slot on bucket %s (timeout=%s)", bucketID, timeout)

	// Inscrever-se em notificações de liberação para este bucket.
	notifyCh, err := s.coordinator.Subscribe(ctx, bucketID)
	if err != nil {
		// Não conseguiu inscrever-se — fazer fallback para polling.
		return s.waitPolling(ctx, bucketID, timeout)
	}

	// Configurar timeout.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// Também fazer polling periodicamente como rede de segurança (caso mensagens Pub/Sub sejam perdidas).
	pollTicker := time.NewTicker(500 * time.Millisecond)
	defer pollTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			metrics.ConnectionsTotal.WithLabelValues(bucketID, "semaphore_cancelled").Inc()
			return ctx.Err()

		case <-timer.C:
			metrics.ConnectionsTotal.WithLabelValues(bucketID, "semaphore_timeout").Inc()
			return fmt.Errorf("semaphore timeout (%v) for bucket %s", timeout, bucketID)

		case _, ok := <-notifyCh:
			if !ok {
				// Canal fechado, mudar para polling.
				return s.waitPolling(ctx, bucketID, timeout-time.Since(start))
			}
			// Uma conexão foi liberada — tentar adquirir.
			if err := s.coordinator.Acquire(ctx, bucketID); err == nil {
				dur := time.Since(start)
				metrics.QueueWaitDuration.WithLabelValues(bucketID).Observe(dur.Seconds())
				log.Printf("[semaphore] Acquired slot on bucket %s after %v", bucketID, dur)
				return nil
			}
			// Alguém pegou primeiro — continuar esperando.

		case <-pollTicker.C:
			// Retry periódico caso tenhamos perdido uma notificação.
			if err := s.coordinator.Acquire(ctx, bucketID); err == nil {
				dur := time.Since(start)
				metrics.QueueWaitDuration.WithLabelValues(bucketID).Observe(dur.Seconds())
				log.Printf("[semaphore] Acquired slot on bucket %s after %v (poll)", bucketID, dur)
				return nil
			}
		}
	}
}

// waitPolling é um fallback que faz polling no Redis por disponibilidade de slot.
func (s *Semaphore) waitPolling(ctx context.Context, bucketID string, remaining time.Duration) error {
	if remaining <= 0 {
		return fmt.Errorf("semaphore timeout for bucket %s", bucketID)
	}

	start := time.Now()
	timer := time.NewTimer(remaining)
	defer timer.Stop()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			metrics.ConnectionsTotal.WithLabelValues(bucketID, "semaphore_timeout").Inc()
			return fmt.Errorf("semaphore timeout (%v) for bucket %s", remaining, bucketID)
		case <-ticker.C:
			if err := s.coordinator.Acquire(ctx, bucketID); err == nil {
				dur := time.Since(start)
				metrics.QueueWaitDuration.WithLabelValues(bucketID).Observe(dur.Seconds())
				return nil
			}
		}
	}
}

// TryAcquire tenta uma única aquisição não-bloqueante.
func (s *Semaphore) TryAcquire(ctx context.Context, bucketID string) error {
	err := s.coordinator.Acquire(ctx, bucketID)
	if err != nil {
		metrics.RedisOperations.WithLabelValues("try_acquire", "rejected").Inc()
	} else {
		metrics.RedisOperations.WithLabelValues("try_acquire", "ok").Inc()
	}
	return err
}
