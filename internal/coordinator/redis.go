// Package coordinator implementa coordenação distribuída via Redis
// para connection pooling entre múltiplas instâncias de proxy.
//
// Fornece:
//   - Acquire/release atômico de slots de conexão usando scripts Lua
//   - Rastreamento de conexões por instância para auditabilidade
//   - Modo fallback quando o Redis está indisponível (limites locais)
//   - Notificações Pub/Sub para wakeup de filas entre instâncias
package coordinator

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/joao-brasil/poc-connection-pooling/internal/config"
	"github.com/joao-brasil/poc-connection-pooling/internal/metrics"
	"github.com/redis/go-redis/v9"
)

//go:embed lua/acquire.lua
var acquireLuaScript string

//go:embed lua/release.lua
var releaseLuaScript string

// ── Padrões de Chaves Redis ──────────────────────────────────────────────
const (
	keyBucketCount  = "proxy:bucket:%s:count"    // contagem global de conexões por bucket
	keyBucketMax    = "proxy:bucket:%s:max"       // máximo de conexões por bucket
	keyInstanceConn = "proxy:instance:%s:conns"   // hash: bucket_id → contagem local
	keyInstanceHB   = "proxy:instance:%s:heartbeat" // chave de heartbeat com TTL
	keyInstanceList = "proxy:instances"            // conjunto de IDs de instâncias ativas
	channelRelease  = "proxy:release:%s"           // canal Pub/Sub por bucket
)

// RedisCoordinator gerencia limites distribuídos de conexão via Redis.
type RedisCoordinator struct {
	client     redis.UniversalClient
	cfg        *config.Config
	instanceID string

	// Hashes SHA dos scripts Lua (carregados uma vez na inicialização).
	acquireSHA string
	releaseSHA string

	// fallback rastreia se o Redis está indisponível e estamos em modo local.
	fallbackMode atomic.Bool

	// fallbackCounts rastreia contagens locais de conexão por bucket em modo fallback.
	fallbackMu     sync.Mutex
	fallbackCounts map[string]int

	// subscribers mantém assinaturas Pub/Sub por bucket.
	subMu       sync.Mutex
	subscribers map[string]*redis.PubSub

	// ciclo de vida
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewRedisCoordinator cria e inicializa o coordenador distribuído.
func NewRedisCoordinator(ctx context.Context, cfg *config.Config) (*RedisCoordinator, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Redis.Addr,
		Password:     cfg.Redis.Password,
		DB:           cfg.Redis.DB,
		PoolSize:     cfg.Redis.PoolSize,
		DialTimeout:  cfg.Redis.DialTimeout,
		ReadTimeout:  cfg.Redis.ReadTimeout,
		WriteTimeout: cfg.Redis.WriteTimeout,
	})

	rc := &RedisCoordinator{
		client:         client,
		cfg:            cfg,
		instanceID:     cfg.Proxy.InstanceID,
		fallbackCounts: make(map[string]int),
		subscribers:    make(map[string]*redis.PubSub),
		stopCh:         make(chan struct{}),
	}

	// Testar conectividade com o Redis.
	pingCtx, cancel := context.WithTimeout(ctx, cfg.Redis.DialTimeout)
	defer cancel()

	if err := client.Ping(pingCtx).Err(); err != nil {
		if cfg.Fallback.Enabled {
			log.Printf("[coordinator] Redis unavailable (%v), starting in fallback mode", err)
			rc.fallbackMode.Store(true)
			metrics.RedisOperations.WithLabelValues("ping", "error").Inc()
			return rc, nil
		}
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}
	metrics.RedisOperations.WithLabelValues("ping", "ok").Inc()
	log.Printf("[coordinator] Redis connected: %s", cfg.Redis.Addr)

	// Carregar scripts Lua.
	if err := rc.loadScripts(ctx); err != nil {
		return nil, fmt.Errorf("loading lua scripts: %w", err)
	}

	// Registrar máximo de conexões por bucket.
	if err := rc.initBucketLimits(ctx); err != nil {
		return nil, fmt.Errorf("initializing bucket limits: %w", err)
	}

	// Registrar esta instância.
	if err := rc.registerInstance(ctx); err != nil {
		return nil, fmt.Errorf("registering instance: %w", err)
	}

	log.Printf("[coordinator] Initialized: instance=%s, %d buckets registered",
		rc.instanceID, len(cfg.Buckets))

	return rc, nil
}

// loadScripts carrega os scripts Lua no Redis e armazena em cache seus hashes SHA.
func (rc *RedisCoordinator) loadScripts(ctx context.Context) error {
	sha, err := rc.client.ScriptLoad(ctx, acquireLuaScript).Result()
	if err != nil {
		return fmt.Errorf("loading acquire.lua: %w", err)
	}
	rc.acquireSHA = sha

	sha, err = rc.client.ScriptLoad(ctx, releaseLuaScript).Result()
	if err != nil {
		return fmt.Errorf("loading release.lua: %w", err)
	}
	rc.releaseSHA = sha

	log.Printf("[coordinator] Lua scripts loaded (acquire=%s..., release=%s...)",
		rc.acquireSHA[:8], rc.releaseSHA[:8])
	return nil
}

// initBucketLimits define a contagem máxima de conexões para cada bucket no Redis.
func (rc *RedisCoordinator) initBucketLimits(ctx context.Context) error {
	pipe := rc.client.Pipeline()
	for _, b := range rc.cfg.Buckets {
		maxKey := fmt.Sprintf(keyBucketMax, b.ID)
		pipe.Set(ctx, maxKey, b.MaxConnections, 0)

		// Inicializar chave de contagem se não existir.
		countKey := fmt.Sprintf(keyBucketCount, b.ID)
		pipe.SetNX(ctx, countKey, 0, 0)
	}
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("pipeline exec: %w", err)
	}
	return nil
}

// registerInstance adiciona esta instância ao conjunto de instâncias ativas.
func (rc *RedisCoordinator) registerInstance(ctx context.Context) error {
	pipe := rc.client.Pipeline()
	pipe.SAdd(ctx, keyInstanceList, rc.instanceID)

	// Inicializar hash de conexões por instância.
	instKey := fmt.Sprintf(keyInstanceConn, rc.instanceID)
	for _, b := range rc.cfg.Buckets {
		pipe.HSetNX(ctx, instKey, b.ID, 0)
	}

	_, err := pipe.Exec(ctx)
	return err
}

// ── Acquire / Release ───────────────────────────────────────────────────

// Acquire incrementa atomicamente a contagem global de conexões de um bucket.
// Retorna nil se o slot foi adquirido, ou um erro se estiver na capacidade máxima ou o Redis falhar.
func (rc *RedisCoordinator) Acquire(ctx context.Context, bucketID string) error {
	if rc.fallbackMode.Load() {
		return rc.acquireFallback(bucketID)
	}

	countKey := fmt.Sprintf(keyBucketCount, bucketID)
	maxKey := fmt.Sprintf(keyBucketMax, bucketID)
	instKey := fmt.Sprintf(keyInstanceConn, rc.instanceID)

	result, err := rc.client.EvalSha(ctx, rc.acquireSHA,
		[]string{countKey, maxKey, instKey},
		bucketID, rc.instanceID,
	).Int64()

	if err != nil {
		metrics.RedisOperations.WithLabelValues("acquire", "error").Inc()
		// Se o Redis falhar, tentar fallback.
		if rc.cfg.Fallback.Enabled {
			log.Printf("[coordinator] Redis acquire failed (%v), falling back to local", err)
			rc.enterFallback()
			return rc.acquireFallback(bucketID)
		}
		return fmt.Errorf("redis acquire: %w", err)
	}

	metrics.RedisOperations.WithLabelValues("acquire", "ok").Inc()

	if result == -1 {
		return fmt.Errorf("bucket %s at max capacity", bucketID)
	}
	if result == -2 {
		return fmt.Errorf("bucket %s max not configured in Redis", bucketID)
	}

	return nil
}

// Release decrementa atomicamente a contagem global de conexões de um bucket
// e publica uma notificação para instâncias em espera.
func (rc *RedisCoordinator) Release(ctx context.Context, bucketID string) error {
	if rc.fallbackMode.Load() {
		rc.releaseFallback(bucketID)
		return nil
	}

	countKey := fmt.Sprintf(keyBucketCount, bucketID)
	instKey := fmt.Sprintf(keyInstanceConn, rc.instanceID)
	channel := fmt.Sprintf(channelRelease, bucketID)

	_, err := rc.client.EvalSha(ctx, rc.releaseSHA,
		[]string{countKey, instKey},
		bucketID, channel,
	).Int64()

	if err != nil {
		metrics.RedisOperations.WithLabelValues("release", "error").Inc()
		if rc.cfg.Fallback.Enabled {
			rc.enterFallback()
			rc.releaseFallback(bucketID)
			return nil
		}
		return fmt.Errorf("redis release: %w", err)
	}

	metrics.RedisOperations.WithLabelValues("release", "ok").Inc()
	return nil
}

// ── Pub/Sub para Notificações Entre Instâncias ─────────────────────────

// Subscribe cria uma assinatura Pub/Sub para notificações de release de um bucket.
// Retorna um channel que recebe o ID do bucket sempre que uma conexão é liberada
// por qualquer instância.
func (rc *RedisCoordinator) Subscribe(ctx context.Context, bucketID string) (<-chan string, error) {
	if rc.fallbackMode.Load() {
		// Em modo fallback, retornar um channel fechado (sem coordenação entre instâncias).
		ch := make(chan string)
		close(ch)
		return ch, nil
	}

	channel := fmt.Sprintf(channelRelease, bucketID)
	sub := rc.client.Subscribe(ctx, channel)

	rc.subMu.Lock()
	rc.subscribers[bucketID] = sub
	rc.subMu.Unlock()

	notifyCh := make(chan string, 16)

	rc.wg.Add(1)
	go func() {
		defer rc.wg.Done()
		defer close(notifyCh)

		ch := sub.Channel()
		for {
			select {
			case <-rc.stopCh:
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				select {
				case notifyCh <- msg.Payload:
				default:
					// Descartar se o consumidor estiver lento (anti-thundering-herd).
				}
			}
		}
	}()

	return notifyCh, nil
}

// ── Modo Fallback ───────────────────────────────────────────────────────

func (rc *RedisCoordinator) enterFallback() {
	if rc.fallbackMode.CompareAndSwap(false, true) {
		log.Printf("[coordinator] Entering fallback mode (local limits)")
		metrics.ConnectionErrors.WithLabelValues("coordinator", "fallback_entered").Inc()
	}
}

// ExitFallback tenta reconectar ao Redis e sair do modo fallback.
func (rc *RedisCoordinator) ExitFallback(ctx context.Context) error {
	if err := rc.client.Ping(ctx).Err(); err != nil {
		return err
	}

	// Recarregar scripts (podem ter sido removidos por flush).
	if err := rc.loadScripts(ctx); err != nil {
		return err
	}

	// Reconciliar: sincronizar contagens locais com o Redis.
	if err := rc.reconcileCounts(ctx); err != nil {
		log.Printf("[coordinator] Reconciliation failed: %v", err)
		// Não sair do fallback se a reconciliação falhar.
		return err
	}

	rc.fallbackMode.Store(false)
	log.Printf("[coordinator] Exited fallback mode, Redis reconnected")
	metrics.ConnectionErrors.WithLabelValues("coordinator", "fallback_exited").Inc()
	return nil
}

// IsFallback retorna true se o coordenador estiver em modo fallback.
func (rc *RedisCoordinator) IsFallback() bool {
	return rc.fallbackMode.Load()
}

func (rc *RedisCoordinator) acquireFallback(bucketID string) error {
	rc.fallbackMu.Lock()
	defer rc.fallbackMu.Unlock()

	localMax := rc.localLimit(bucketID)
	current := rc.fallbackCounts[bucketID]

	if current >= localMax {
		return fmt.Errorf("bucket %s at local fallback limit (%d/%d)",
			bucketID, current, localMax)
	}

	rc.fallbackCounts[bucketID] = current + 1
	return nil
}

func (rc *RedisCoordinator) releaseFallback(bucketID string) {
	rc.fallbackMu.Lock()
	defer rc.fallbackMu.Unlock()

	if rc.fallbackCounts[bucketID] > 0 {
		rc.fallbackCounts[bucketID]--
	}
}

// localLimit calcula o limite de conexões por instância para o modo fallback.
func (rc *RedisCoordinator) localLimit(bucketID string) int {
	for _, b := range rc.cfg.Buckets {
		if b.ID == bucketID {
			divisor := rc.cfg.Fallback.LocalLimitDivisor
			if divisor <= 0 {
				divisor = 3
			}
			limit := b.MaxConnections / divisor
			if limit < 1 {
				limit = 1
			}
			return limit
		}
	}
	return 1
}

// reconcileCounts sincroniza contagens locais do fallback com o Redis após reconexão.
func (rc *RedisCoordinator) reconcileCounts(ctx context.Context) error {
	rc.fallbackMu.Lock()
	counts := make(map[string]int, len(rc.fallbackCounts))
	for k, v := range rc.fallbackCounts {
		counts[k] = v
	}
	rc.fallbackMu.Unlock()

	pipe := rc.client.Pipeline()
	instKey := fmt.Sprintf(keyInstanceConn, rc.instanceID)

	for bucketID, count := range counts {
		pipe.HSet(ctx, instKey, bucketID, count)
	}

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("reconcile pipeline: %w", err)
	}

	log.Printf("[coordinator] Reconciled %d bucket counts to Redis", len(counts))
	return nil
}

// ── Métodos de Consulta ─────────────────────────────────────────────────

// GlobalCount retorna a contagem global atual de conexões de um bucket.
func (rc *RedisCoordinator) GlobalCount(ctx context.Context, bucketID string) (int, error) {
	if rc.fallbackMode.Load() {
		rc.fallbackMu.Lock()
		defer rc.fallbackMu.Unlock()
		return rc.fallbackCounts[bucketID], nil
	}

	countKey := fmt.Sprintf(keyBucketCount, bucketID)
	val, err := rc.client.Get(ctx, countKey).Int()
	if err == redis.Nil {
		return 0, nil
	}
	return val, err
}

// InstanceCounts retorna as contagens de conexão por bucket para uma instância específica.
func (rc *RedisCoordinator) InstanceCounts(ctx context.Context, instanceID string) (map[string]int, error) {
	instKey := fmt.Sprintf(keyInstanceConn, instanceID)
	result, err := rc.client.HGetAll(ctx, instKey).Result()
	if err != nil {
		return nil, err
	}

	counts := make(map[string]int, len(result))
	for k, v := range result {
		var n int
		fmt.Sscanf(v, "%d", &n)
		counts[k] = n
	}
	return counts, nil
}

// ActiveInstances retorna o conjunto de IDs de instâncias ativas.
func (rc *RedisCoordinator) ActiveInstances(ctx context.Context) ([]string, error) {
	return rc.client.SMembers(ctx, keyInstanceList).Result()
}

// ── Ciclo de Vida ───────────────────────────────────────────────────────

// Close encerra o coordenador, desregistra a instância e fecha a conexão Redis.
func (rc *RedisCoordinator) Close(ctx context.Context) error {
	close(rc.stopCh)

	// Fechar todas as assinaturas Pub/Sub.
	rc.subMu.Lock()
	for _, sub := range rc.subscribers {
		sub.Close()
	}
	rc.subscribers = nil
	rc.subMu.Unlock()

	rc.wg.Wait()

	// Desregistrar instância.
	if !rc.fallbackMode.Load() {
		rc.client.SRem(ctx, keyInstanceList, rc.instanceID)
		instKey := fmt.Sprintf(keyInstanceConn, rc.instanceID)
		rc.client.Del(ctx, instKey)
		hbKey := fmt.Sprintf(keyInstanceHB, rc.instanceID)
		rc.client.Del(ctx, hbKey)
	}

	log.Printf("[coordinator] Instance %s unregistered", rc.instanceID)
	return rc.client.Close()
}

// Client retorna o cliente Redis subjacente (para heartbeat e outros usos internos).
func (rc *RedisCoordinator) Client() redis.UniversalClient {
	return rc.client
}

// InstanceID retorna o ID de instância deste coordenador.
func (rc *RedisCoordinator) InstanceID() string {
	return rc.instanceID
}
