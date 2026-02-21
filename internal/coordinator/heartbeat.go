package coordinator

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/joao-brasil/poc-connection-pooling/internal/metrics"
)

// Heartbeat atualiza periodicamente a presença desta instância no Redis
// e detecta/limpa instâncias mortas cujas conexões não foram liberadas.
type Heartbeat struct {
	coordinator *RedisCoordinator
	interval    time.Duration
	ttl         time.Duration
	stopCh      chan struct{}
}

// NewHeartbeat cria um worker de heartbeat para o coordinator fornecido.
func NewHeartbeat(rc *RedisCoordinator) *Heartbeat {
	interval := rc.cfg.Redis.HeartbeatInterval
	if interval == 0 {
		interval = 10 * time.Second
	}
	ttl := rc.cfg.Redis.HeartbeatTTL
	if ttl == 0 {
		ttl = 30 * time.Second
	}

	return &Heartbeat{
		coordinator: rc,
		interval:    interval,
		ttl:         ttl,
		stopCh:      make(chan struct{}),
	}
}

// Start inicia o loop de heartbeat em uma goroutine em background.
func (hb *Heartbeat) Start(ctx context.Context) {
	hb.coordinator.wg.Add(1)
	go hb.loop(ctx)
	log.Printf("[heartbeat] Started: interval=%s, ttl=%s, instance=%s",
		hb.interval, hb.ttl, hb.coordinator.instanceID)
}

// Stop sinaliza para o loop de heartbeat parar.
func (hb *Heartbeat) Stop() {
	close(hb.stopCh)
}

// loop executa o heartbeat periódico e a limpeza de instâncias mortas.
func (hb *Heartbeat) loop(ctx context.Context) {
	defer hb.coordinator.wg.Done()

	// Enviar heartbeat inicial imediatamente.
	hb.sendHeartbeat(ctx)

	ticker := time.NewTicker(hb.interval)
	defer ticker.Stop()

	// Executar limpeza com menos frequência (a cada 3 intervalos).
	cleanupCounter := 0

	for {
		select {
		case <-hb.stopCh:
			return
		case <-hb.coordinator.stopCh:
			return
		case <-ticker.C:
			if hb.coordinator.IsFallback() {
				// Tentar reconectar ao Redis.
				if err := hb.coordinator.ExitFallback(ctx); err != nil {
					continue
				}
			}

			hb.sendHeartbeat(ctx)

			cleanupCounter++
			if cleanupCounter%3 == 0 {
				hb.cleanupDeadInstances(ctx)
			}
		}
	}
}

// sendHeartbeat atualiza a chave de heartbeat desta instância com um TTL.
func (hb *Heartbeat) sendHeartbeat(ctx context.Context) {
	if hb.coordinator.IsFallback() {
		return
	}

	hbKey := fmt.Sprintf(keyInstanceHB, hb.coordinator.instanceID)
	err := hb.coordinator.client.Set(ctx, hbKey, time.Now().Unix(), hb.ttl).Err()
	if err != nil {
		log.Printf("[heartbeat] Failed to send heartbeat: %v", err)
		metrics.RedisOperations.WithLabelValues("heartbeat", "error").Inc()
		return
	}

	metrics.InstanceHeartbeat.WithLabelValues(hb.coordinator.instanceID).Set(1)
	metrics.RedisOperations.WithLabelValues("heartbeat", "ok").Inc()
}

// cleanupDeadInstances verifica instâncias cujo heartbeat expirou
// e reconcilia suas contagens de conexões órfãs.
func (hb *Heartbeat) cleanupDeadInstances(ctx context.Context) {
	if hb.coordinator.IsFallback() {
		return
	}

	// Obter todas as instâncias registradas.
	instances, err := hb.coordinator.client.SMembers(ctx, keyInstanceList).Result()
	if err != nil {
		log.Printf("[heartbeat] Failed to list instances: %v", err)
		return
	}

	for _, instID := range instances {
		if instID == hb.coordinator.instanceID {
			continue // pular nós mesmos
		}

		// Verificar se o heartbeat da instância ainda está vivo.
		hbKey := fmt.Sprintf(keyInstanceHB, instID)
		exists, err := hb.coordinator.client.Exists(ctx, hbKey).Result()
		if err != nil {
			continue
		}

		if exists > 0 {
			continue // instância está viva
		}

		// Instância está morta — limpar suas conexões órfãs.
		log.Printf("[heartbeat] Instance %s appears dead (no heartbeat), cleaning up", instID)
		hb.cleanupInstance(ctx, instID)
	}
}

// cleanupInstance remove as contagens de conexões de uma instância dos totais globais.
func (hb *Heartbeat) cleanupInstance(ctx context.Context, deadInstanceID string) {
	instKey := fmt.Sprintf(keyInstanceConn, deadInstanceID)

	// Ler as contagens de conexões por bucket da instância morta.
	counts, err := hb.coordinator.client.HGetAll(ctx, instKey).Result()
	if err != nil {
		log.Printf("[heartbeat] Failed to read counts for dead instance %s: %v", deadInstanceID, err)
		return
	}

	// Subtrair as contagens da instância morta dos contadores globais.
	pipe := hb.coordinator.client.Pipeline()
	totalRecovered := 0

	for bucketID, countStr := range counts {
		count, err := strconv.Atoi(countStr)
		if err != nil || count <= 0 {
			continue
		}

		countKey := fmt.Sprintf(keyBucketCount, bucketID)
		pipe.DecrBy(ctx, countKey, int64(count))
		totalRecovered += count
	}

	// Remover os dados da instância morta.
	pipe.Del(ctx, instKey)
	pipe.SRem(ctx, keyInstanceList, deadInstanceID)

	_, err = pipe.Exec(ctx)
	if err != nil {
		log.Printf("[heartbeat] Failed to cleanup dead instance %s: %v", deadInstanceID, err)
		return
	}

	if totalRecovered > 0 {
		log.Printf("[heartbeat] Cleaned up dead instance %s: recovered %d connection slots",
			deadInstanceID, totalRecovered)
		metrics.ConnectionErrors.WithLabelValues("coordinator", "dead_instance_cleanup").Inc()
	}

	// Garantir que contagens globais não fiquem abaixo de zero.
	for bucketID := range counts {
		countKey := fmt.Sprintf(keyBucketCount, bucketID)
		val, err := hb.coordinator.client.Get(ctx, countKey).Int64()
		if err == nil && val < 0 {
			hb.coordinator.client.Set(ctx, countKey, 0, 0)
			log.Printf("[heartbeat] Corrected negative count for bucket %s", bucketID)
		}
	}
}
