# CONTRACTS.md — API Pública, Structs e Constantes

> **Propósito:** evitar abrir arquivos só para consultar assinaturas, tipos e constantes.
> Contém TUDO que é público + tipos internos relevantes para integração entre pacotes.
>
> **Última atualização:** 2026-02-18 (Fase 4 concluída)

---

## 1. `pkg/bucket` — Modelo de Bucket

```go
// bucket.go (50 loc)

type Bucket struct {
    ID                string        `yaml:"id"`
    Host              string        `yaml:"host"`
    Port              int           `yaml:"port"`
    Database          string        `yaml:"database"`
    Username          string        `yaml:"username"`
    Password          string        `yaml:"password"`
    MaxConnections    int           `yaml:"max_connections"`
    MinIdle           int           `yaml:"min_idle"`
    MaxIdleTime       time.Duration `yaml:"max_idle_time"`
    ConnectionTimeout time.Duration `yaml:"connection_timeout"`
    QueueTimeout      time.Duration `yaml:"queue_timeout"`
}

func (b *Bucket) DSN() string     // "sqlserver://user:pass@host:port?database=...&connection+timeout=..."
func (b *Bucket) Addr() string    // "host:port"
```

---

## 2. `internal/config` — Configuração

```go
// config.go (219 loc)

type ProxyConfig struct {
    ListenAddr          string        `yaml:"listen_addr"`          // default "0.0.0.0"
    ListenPort          int           `yaml:"listen_port"`          // obrigatório
    InstanceID          string        `yaml:"instance_id"`          // default hostname
    SessionTimeout      time.Duration `yaml:"session_timeout"`      // default 5m
    IdleTimeout         time.Duration `yaml:"idle_timeout"`         // default 60s
    QueueTimeout        time.Duration `yaml:"queue_timeout"`        // default 30s
    MaxQueueSize        int           `yaml:"max_queue_size"`        // default 1000 (0 = unlimited)
    PinningMode         string        `yaml:"pinning_mode"`         // default "transaction"
    HealthCheckInterval time.Duration `yaml:"health_check_interval"`// default 15s
    HealthCheckPort     int           `yaml:"health_check_port"`    // default 8080
    MetricsPort         int           `yaml:"metrics_port"`         // default 9090
}

type RedisConfig struct {
    Addr              string        `yaml:"addr"`               // default "redis:6379"
    Password          string        `yaml:"password"`
    DB                int           `yaml:"db"`
    PoolSize          int           `yaml:"pool_size"`           // default 20
    DialTimeout       time.Duration `yaml:"dial_timeout"`        // default 5s
    ReadTimeout       time.Duration `yaml:"read_timeout"`        // default 3s
    WriteTimeout      time.Duration `yaml:"write_timeout"`       // default 3s
    HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`  // default 10s
    HeartbeatTTL      time.Duration `yaml:"heartbeat_ttl"`       // default 30s
}

type FallbackConfig struct {
    Enabled           bool `yaml:"enabled"`
    LocalLimitDivisor int  `yaml:"local_limit_divisor"` // default 3
}

type Config struct {
    Proxy    ProxyConfig
    Redis    RedisConfig
    Fallback FallbackConfig
    Buckets  []bucket.Bucket
}

func Load(proxyConfigPath, bucketsConfigPath string) (*Config, error)
func (c *Config) BucketByID(id string) (*bucket.Bucket, bool)
func (c *Config) BucketByDatabase(database string) (*bucket.Bucket, bool)
```

---

## 3. `internal/coordinator` — Coordenação Distribuída

### 3.1 RedisCoordinator (`redis.go`, 479 loc)

```go
type RedisCoordinator struct {
    client         redis.UniversalClient
    cfg            *config.Config
    instanceID     string
    acquireSHA     string          // SHA do acquire.lua
    releaseSHA     string          // SHA do release.lua
    fallbackMode   atomic.Bool
    fallbackMu     sync.Mutex
    fallbackCounts map[string]int  // bucketID → count local
    subMu          sync.Mutex
    subscribers    map[string]*redis.PubSub
    stopCh         chan struct{}
    wg             sync.WaitGroup
}

// Lifecycle
func NewRedisCoordinator(ctx context.Context, cfg *config.Config) (*RedisCoordinator, error)
func (rc *RedisCoordinator) Close(ctx context.Context) error

// Core — chamados por proxy/handler.go a cada sessão
func (rc *RedisCoordinator) Acquire(ctx context.Context, bucketID string) error   // nil=ok, err=lotado/falha
func (rc *RedisCoordinator) Release(ctx context.Context, bucketID string) error

// Pub/Sub — usado pelo Semaphore
func (rc *RedisCoordinator) Subscribe(ctx context.Context, bucketID string) (<-chan string, error)

// Fallback
func (rc *RedisCoordinator) IsFallback() bool
func (rc *RedisCoordinator) ExitFallback(ctx context.Context) error

// Queries
func (rc *RedisCoordinator) GlobalCount(ctx context.Context, bucketID string) (int, error)
func (rc *RedisCoordinator) InstanceCounts(ctx context.Context, instanceID string) (map[string]int, error)
func (rc *RedisCoordinator) ActiveInstances(ctx context.Context) ([]string, error)

// Internos expostos para heartbeat
func (rc *RedisCoordinator) Client() redis.UniversalClient
func (rc *RedisCoordinator) InstanceID() string
```

### 3.2 Heartbeat (`heartbeat.go`, 196 loc)

```go
type Heartbeat struct {
    coordinator *RedisCoordinator
    interval    time.Duration     // default 10s
    ttl         time.Duration     // default 30s
    stopCh      chan struct{}
}

func NewHeartbeat(rc *RedisCoordinator) *Heartbeat
func (hb *Heartbeat) Start(ctx context.Context)  // spawna goroutine
func (hb *Heartbeat) Stop()
```

**Comportamento do loop:**
- A cada `interval`: envia heartbeat (`SET key TTL`)
- A cada `3 × interval`: executa `cleanupDeadInstances`
  - Lista `SMEMBERS proxy:instances`
  - Para cada (exceto self): `EXISTS heartbeat key`
  - Se ausente: `HGETALL` counts → `DECRBY` global → `DEL` + `SREM`
  - Corrige contadores negativos
- Se em fallback: tenta `ExitFallback()`

### 3.3 Semaphore (`semaphore.go`, 135 loc)

```go
type Semaphore struct {
    coordinator *RedisCoordinator
}

func NewSemaphore(rc *RedisCoordinator) *Semaphore
func (s *Semaphore) Wait(ctx context.Context, bucketID string, timeout time.Duration) error
func (s *Semaphore) TryAcquire(ctx context.Context, bucketID string) error
```

**Wait:** fast-path `Acquire` → subscribe Pub/Sub → loop `select` com notify/poll(500ms)/timer/ctx.

---

## 4. `internal/queue` — Fila Distribuída

```go
// distributed.go (~200 loc)

type DistributedQueue struct {
    coordinator  *coordinator.RedisCoordinator
    semaphore    *coordinator.Semaphore
    mu           sync.Mutex
    depths       map[string]int   // bucketID → waiters count
    timeout      time.Duration    // default 30s
    maxQueueSize int              // 0 = unlimited (Phase 4)
}

func NewDistributedQueue(rc *coordinator.RedisCoordinator, timeout time.Duration, maxQueueSize int) *DistributedQueue
func (dq *DistributedQueue) Acquire(ctx context.Context, bucketID string) error  // TryAcquire → circuit breaker → Wait
func (dq *DistributedQueue) Release(ctx context.Context, bucketID string) error  // → coordinator.Release
func (dq *DistributedQueue) Depth(bucketID string) int

// Error types (Phase 4)
type QueueErrorKind int
const (
    QueueErrorTimeout QueueErrorKind = iota  // waited full timeout
    QueueErrorFull                           // circuit breaker (max queue size)
)

type QueueError struct {
    BucketID string
    Kind     QueueErrorKind
    Depth    int              // for QueueErrorFull
    MaxSize  int              // for QueueErrorFull
    WaitTime time.Duration    // for QueueErrorTimeout
    Timeout  time.Duration    // for QueueErrorTimeout
}

func (e *QueueError) Error() string
func IsQueueFull(err error) bool     // checks Kind == QueueErrorFull
func IsQueueTimeout(err error) bool  // checks Kind == QueueErrorTimeout
```

---

## 5. `internal/pool` — Connection Pool Local

### 5.1 PooledConn (`connection.go`, 175 loc)

```go
type PinReason string
const (
    PinNone        PinReason = ""
    PinTransaction PinReason = "transaction"
    PinPrepared    PinReason = "prepared"
    PinBulkLoad    PinReason = "bulk_load"
)

type ConnState int
const (
    ConnStateIdle   ConnState = iota
    ConnStateActive
    ConnStateClosed
)

type PooledConn struct {
    db              *sql.DB       // 1:1 com conexão física (MaxOpenConns=1)
    id              uint64
    bucketID        string
    state           ConnState
    pinReason       PinReason
    pinnedAt        time.Time
    createdAt       time.Time
    lastUsedAt      time.Time
    lastHealthCheck time.Time
    useCount        uint64
}

func (c *PooledConn) DB() *sql.DB
func (c *PooledConn) ID() uint64
func (c *PooledConn) BucketID() string
func (c *PooledConn) State() ConnState
func (c *PooledConn) IsPinned() bool
func (c *PooledConn) PinReason() PinReason
func (c *PooledConn) Pin(reason PinReason)
func (c *PooledConn) Unpin() time.Duration   // retorna duração do pin
func (c *PooledConn) Close() error
```

### 5.2 BucketPool (`pool.go`, 443 loc)

```go
type BucketPool struct {
    mu      sync.Mutex
    bucket  *bucket.Bucket
    idle    []*PooledConn           // LIFO stack
    active  map[uint64]*PooledConn
    nextID  atomic.Uint64
    closed  bool
    waiters []chan *PooledConn      // channel-based wait queue
    notify  chan struct{}
    stopCh  chan struct{}
    wg      sync.WaitGroup
}

func NewBucketPool(ctx context.Context, b *bucket.Bucket) (*BucketPool, error)
func (bp *BucketPool) Acquire(ctx context.Context) (*PooledConn, error)
func (bp *BucketPool) Release(conn *PooledConn)     // sp_reset_connection → idle ou hand-off waiter
func (bp *BucketPool) Discard(conn *PooledConn)      // fecha e remove permanentemente
func (bp *BucketPool) Close() error
func (bp *BucketPool) Stats() PoolStats
func (bp *BucketPool) HealthCheck()                  // PingContext em todas idle conns

type PoolStats struct {
    BucketID  string
    Active    int
    Idle      int
    Max       int
    WaitQueue int
}
```

**Acquire flow:** idle pop → create if under max → wait queue (channel + timeout) \
**Release flow:** delete active → sp_reset_connection → hand-off waiter se houver → push idle \
**Maintenance (30s):** evictStale (MaxIdleTime) → ensureMinIdle

### 5.3 Manager (`manager.go`, 134 loc)

```go
type Manager struct {
    mu    sync.RWMutex
    pools map[string]*BucketPool
    cfg   *config.Config
}

func NewManager(ctx context.Context, cfg *config.Config) (*Manager, error)
func (m *Manager) Acquire(ctx context.Context, bucketID string) (*PooledConn, error)
func (m *Manager) AcquireForBucket(ctx context.Context, b *bucket.Bucket) (*PooledConn, error)
func (m *Manager) Release(conn *PooledConn)
func (m *Manager) Discard(conn *PooledConn)
func (m *Manager) Stats() []PoolStats
func (m *Manager) Pool(bucketID string) (*BucketPool, bool)
func (m *Manager) Close() error
```

---

## 6. `internal/proxy` — TDS Proxy

### 6.1 Server (`listener.go`, ~165 loc)

```go
type Server struct {
    cfg            *config.Config
    poolMgr        *pool.Manager
    coordinator    *coordinator.RedisCoordinator   // pode ser nil (pre-Fase 3)
    dqueue         *queue.DistributedQueue          // Phase 4
    router         *Router
    listener       net.Listener
    activeSessions atomic.Int64
    done           chan struct{}
    wg             sync.WaitGroup
    cancel         context.CancelFunc
}

func NewServer(cfg *config.Config, poolMgr *pool.Manager, rc *coordinator.RedisCoordinator, dq *queue.DistributedQueue) *Server
func (s *Server) Start(ctx context.Context) error
func (s *Server) Stop(ctx context.Context) error
```

### 6.2 Session (`handler.go`, ~310 loc)

```go
type Session struct {
    id           uint64
    clientConn   net.Conn
    cfg          *config.Config
    poolMgr      *pool.Manager
    coordinator  *coordinator.RedisCoordinator
    dqueue       *queue.DistributedQueue   // Phase 4
    router       *Router
    bucketID     string
    backendConn  net.Conn
    poolConn     *pool.PooledConn
    slotAcquired bool          // true se dqueue.Acquire foi chamado
    pinned       bool
    pinReason    string
    startedAt    time.Time
}

// Lifecycle (chamado pelo accept loop)
func (s *Session) Handle(ctx context.Context)
```

**Handle flow (Phase 4 atualizado):**
1. Read client Pre-Login → parse encryption flag
2. `pickBucket()` → seleciona bucket (atualmente: primeiro bucket)
3. `dqueue.Acquire(bucketID)` → TryAcquire → circuit breaker → Semaphore.Wait
   - Queue full → `tds.ErrQueueFull` (50005)
   - Timeout → `tds.ErrQueueTimeout` (50004)
   - Cancelled → `tds.ErrBackendUnavailable`
4. `net.DialTimeout` → conecta ao backend SQL Server
5. Forward Pre-Login → relay resposta
6. `io.Copy` bidirecional (TCP relay transparente)
7. `cleanup()` → close conns + `dqueue.Release(bucketID)`

### 6.3 Router (`router.go`, 136 loc)

```go
type Router struct {
    cfg           *config.Config
    byDatabase    map[string]*bucket.Bucket
    byServerName  map[string]*bucket.Bucket
    byHost        map[string]*bucket.Bucket
    byID          map[string]*bucket.Bucket
    defaultBucket *bucket.Bucket
}

func NewRouter(cfg *config.Config) *Router
func (r *Router) Route(login7 *tds.Login7Info) (*bucket.Bucket, error)
```

**Route priority:** serverName → database (se único) → username match → default (1 bucket)

⚠️ **Router NÃO é usado no fluxo atual** — bucket é escolhido por `pickBucket()` antes do Login7.

---

## 7. `internal/tds` — Parser TDS

### 7.1 Constantes e Tipos Base (`packet.go`, 266 loc)

```go
type PacketType byte
const (
    PacketSQLBatch   PacketType = 0x01
    PacketRPCRequest PacketType = 0x03
    PacketReply      PacketType = 0x04
    PacketAttention  PacketType = 0x06
    PacketBulkLoad   PacketType = 0x07
    PacketTransMgr   PacketType = 0x0E
    PacketPreLogin   PacketType = 0x12
    PacketLogin7     PacketType = 0x10
    PacketSSPI       PacketType = 0x11
)

const (
    StatusNormal        byte = 0x00
    StatusEOM           byte = 0x01
    StatusResetConn     byte = 0x08
    StatusResetConnSkip byte = 0x10
)

const HeaderSize = 8
const MaxPacketSize = 32768

type Header struct {
    Type     PacketType
    Status   byte
    Length   uint16    // total incluindo header, big-endian
    SPID     uint16
    PacketID byte
    Window   byte
}

func (h *Header) IsEOM() bool
func (h *Header) PayloadLength() int
func (h *Header) Marshal() []byte

func ReadHeader(r io.Reader) (*Header, error)
func ParseHeader(buf []byte) (*Header, error)
func ReadPacket(r io.Reader) (*Header, []byte, error)
func ReadMessage(r io.Reader) (PacketType, []byte, [][]byte, error)  // até EOM
func WritePackets(w io.Writer, packets [][]byte) error
func BuildPackets(pktType PacketType, payload []byte, packetSize int) [][]byte
```

### 7.2 Pre-Login (`prelogin.go`, 209 loc)

```go
const (
    EncryptOff    byte = 0x00
    EncryptOn     byte = 0x01
    EncryptNotSup byte = 0x02
    EncryptReq    byte = 0x03
)

type PreLoginMsg struct {
    Options []PreLoginOption
}

func ParsePreLogin(payload []byte) (*PreLoginMsg, error)
func (m *PreLoginMsg) Encryption() byte
func (m *PreLoginMsg) SetEncryption(enc byte)
func (m *PreLoginMsg) Marshal() []byte
func BuildPreLoginResponse(clientPreLogin *PreLoginMsg) []byte
func ForwardPreLogin(client io.ReadWriter, backend io.ReadWriter) (*PreLoginMsg, error)
```

### 7.3 Login7 (`login7.go`, 173 loc)

```go
type Login7Info struct {
    TDSVersion          uint32
    HostName             string
    UserName             string
    AppName              string
    ServerName           string
    Database             string
    ClientInterfaceName  string
}

func ParseLogin7(payload []byte) (*Login7Info, error)
```

**Layout:** header fixo 72 bytes + campos variáveis via offset/length pairs (UTF-16 LE).

### 7.4 Pinning (`pinning.go`, 374 loc)

```go
type PinAction int
const (
    PinActionNone   PinAction = iota
    PinActionPin
    PinActionUnpin
)

type PinResult struct {
    Action PinAction
    Reason string
}

func InspectPacket(pktType PacketType, payload []byte) PinResult
func InspectResponse(payload []byte) PinResult
func IsAttention(pktType PacketType) bool
func BuildAttention() []byte
func ContainsAttentionAck(payload []byte) bool
```

**InspectPacket detecta:**
| Tipo Pacote     | Pin                                  | Unpin                      |
|-----------------|--------------------------------------|----------------------------|
| SQL_BATCH       | BEGIN TRAN, SET IMPLICIT_TRANSACTIONS, CREATE #TABLE | COMMIT, ROLLBACK |
| RPC_REQUEST     | sp_prepare, sp_cursoropen            | sp_unprepare, sp_cursorclose |
| TRANS_MGR       | TM_BEGIN_XACT                        | TM_COMMIT, TM_ROLLBACK     |
| BULK_LOAD       | sempre pin                           | —                          |

**InspectResponse detecta:** ENVCHANGE token type 8 (begin txn) → pin, type 9/10 (commit/rollback) → unpin.

⚠️ **Pinning NÃO está ativado** — o proxy usa `io.Copy` e não chama `InspectPacket`.

### 7.5 Relay (`relay.go`, 173 loc)

```go
type PacketCallback func(direction string, pktType PacketType, payload []byte) error

func Relay(client io.ReadWriter, backend io.ReadWriter, callback PacketCallback) error
func RelayMessage(src io.Reader, dst io.Writer) (PacketType, []byte, error)
func RelayUntilEOM(src io.Reader, dst io.Writer) ([][]byte, error)
func DrainResponse(r io.Reader) error
func ForwardLogin7(client io.Reader, backend io.Writer) (*Login7Info, error)
```

⚠️ `Relay` com callback é o mecanismo para ativar pinning — substituiria `io.Copy`.

### 7.6 Error (`error.go`, ~210 loc)

```go
func BuildErrorResponse(msgNumber uint32, severity uint8, message, serverName string) []byte

// Erros pré-construídos
func ErrPoolExhausted(bucketID string) []byte      // 50001, severity 16
func ErrRoutingFailed(database string) []byte       // 50002, severity 16
func ErrBackendUnavailable(bucketID string) []byte  // 50003, severity 20 (fatal)
func ErrInternalError(message string) []byte        // 50000, severity 16
func ErrQueueTimeout(bucketID string) []byte        // 50004, severity 16 (Phase 4)
func ErrQueueFull(bucketID string) []byte           // 50005, severity 16 (Phase 4)
```

---

## 8. `internal/health` — Health Checker

```go
// health.go (265 loc)

type Status string
const (
    StatusHealthy   Status = "healthy"
    StatusUnhealthy Status = "unhealthy"
)

type ComponentHealth struct {
    Name    string `json:"name"`
    Status  Status `json:"status"`
    Message string `json:"message,omitempty"`
    Latency string `json:"latency"`
}

type HealthReport struct {
    Status     Status            `json:"status"`
    Timestamp  string            `json:"timestamp"`
    InstanceID string            `json:"instance_id"`
    Components []ComponentHealth `json:"components"`
}

type Checker struct { /* cfg, redisClient */ }

func NewChecker(cfg *config.Config) *Checker
func (c *Checker) Check(ctx context.Context) *HealthReport
func (c *Checker) ServeHTTP(ctx context.Context) *http.Server  // :8080
func (c *Checker) Close() error
```

**Endpoints:**
- `GET /health` — full check (Redis + todos SQL Servers)
- `GET /health/ready` — mesmo que /health
- `GET /health/live` — responde 200 sempre (usado pelo HAProxy)

---

## 9. `internal/metrics` — Prometheus

```go
// metrics.go (92 loc) — tudo registrado via promauto, pronto para uso

var ConnectionsActive  *prometheus.GaugeVec     // labels: bucket_id
var ConnectionsIdle    *prometheus.GaugeVec     // labels: bucket_id
var ConnectionsPinned  *prometheus.GaugeVec     // labels: bucket_id, pin_reason
var ConnectionsMax     *prometheus.GaugeVec     // labels: bucket_id
var ConnectionsTotal   *prometheus.CounterVec   // labels: bucket_id, status
var QueueLength        *prometheus.GaugeVec     // labels: bucket_id
var QueueWaitDuration  *prometheus.HistogramVec // labels: bucket_id
var TDSPacketsTotal    *prometheus.CounterVec   // labels: bucket_id, direction, type
var QueryDuration      *prometheus.HistogramVec // labels: bucket_id
var ConnectionErrors   *prometheus.CounterVec   // labels: bucket_id, error_type
var RedisOperations    *prometheus.CounterVec   // labels: operation, status
var InstanceHeartbeat  *prometheus.GaugeVec     // labels: instance_id
var PinningDuration    *prometheus.HistogramVec // labels: bucket_id, pin_reason
```

**Métricas ainda não populadas** (preparadas para fases futuras):
- `TDSPacketsTotal` — precisa de `tds.Relay` com callback
- `QueryDuration` — precisa de parsing TDS para medir request/response
- `PinningDuration` — precisa de pinning ativado

**Métricas populadas na Fase 4:**
- `QueueLength` — `DistributedQueue.incrementDepth/decrementDepth`
- `QueueWaitDuration` — `Semaphore.Wait` (ao adquirir após espera)
- `ConnectionsTotal` — novos status: `acquired`, `acquired_after_wait`, `timeout`, `cancelled`, `rejected_queue_full`
- `ConnectionErrors` — novos tipos: `queue_full`, `queue_timeout`

---

## 10. Lua Scripts — Contratos Redis

### acquire.lua
```
KEYS[1] = proxy:bucket:{id}:count       (string, global count)
KEYS[2] = proxy:bucket:{id}:max         (string, max allowed)
KEYS[3] = proxy:instance:{inst}:conns   (hash, bucket→local count)
ARGV[1] = bucket_id
ARGV[2] = instance_id

Retorno (int64):
  >0  → novo count global (sucesso)
  -1  → pool lotado (current >= max)
  -2  → max não configurado
```

### release.lua
```
KEYS[1] = proxy:bucket:{id}:count
KEYS[2] = proxy:instance:{inst}:conns
ARGV[1] = bucket_id
ARGV[2] = channel name (proxy:release:{bucket_id})

Retorno (int64):
  >=0 → novo count global
  -1  → underflow (count já era 0)

Efeito colateral: PUBLISH channel bucket_id
```

---

## 11. `cmd/proxy/main.go` — Sequência de Inicialização

```
1. flag.Parse()
2. config.Load(proxy.yaml, buckets.yaml)
3. Métricas HTTP :9090/metrics
4. health.NewChecker → ServeHTTP :8080
5. health.Check() — log do resultado
6. pool.NewManager() — 3 BucketPools × 5 idle connections
7. coordinator.NewRedisCoordinator() — connect, Lua scripts, register instance
8. coordinator.NewHeartbeat().Start() — goroutine background
9. queue.NewDistributedQueue(rc, timeout, maxQueueSize) — Phase 4
10. proxy.NewServer(cfg, poolMgr, coordinator, dqueue).Start() — TCP :1433
11. <- SIGINT/SIGTERM
12. Shutdown: metrics.heartbeat=0 → health.Shutdown → metrics.Shutdown → health.Close → pool.Close → proxy.Stop → coordinator.Close
```

---

## 12. Referência Rápida — Quem Chama Quem

```
main.go
  │
  ├── config.Load() ──────────────────────────── retorna *Config
  ├── health.NewChecker(cfg)
  ├── pool.NewManager(ctx, cfg) ──────────────── cria BucketPool por bucket
  │     └── BucketPool.createConn() ──────────── sql.Open("sqlserver", dsn)
  ├── coordinator.NewRedisCoordinator(ctx, cfg)
  │     ├── redis.NewClient()
  │     ├── client.ScriptLoad(acquire.lua)
  │     ├── client.ScriptLoad(release.lua)
  │     ├── pipeline: SET max, SETNX count
  │     └── pipeline: SADD instances, HSetNX conns
  ├── coordinator.NewHeartbeat(rc).Start(ctx)
  │     └── loop: SET heartbeat TTL + cleanupDeadInstances
  ├── queue.NewDistributedQueue(rc, timeout, maxQueueSize) ── Phase 4
  └── proxy.NewServer(cfg, poolMgr, rc, dq).Start()
        └── acceptLoop → newSession → Session.Handle()
              ├── tds.ReadMessage() ─────────── Pre-Login do client
              ├── tds.ParsePreLogin()
              ├── pickBucket() ──────────────── seleciona bucket (bucket[0])
              ├── dq.Acquire(ctx, bucketID) ── Phase 4 flow:
              │     ├── TryAcquire ──────────── EVALSHA acquire.lua
              │     ├── circuit breaker ─────── depth >= maxQueueSize? → QueueError{Full}
              │     └── Semaphore.Wait ──────── Pub/Sub + poll → EVALSHA acquire.lua
              │           └── timeout ──────── QueueError{Timeout}
              ├── net.DialTimeout() ─────────── TCP para backend
              ├── tds.WritePackets() ────────── forward Pre-Login
              ├── tds.ReadMessage() ─────────── resposta backend
              ├── tds.WritePackets() ────────── relay para client
              ├── io.Copy (bidirecional) ────── dados TLS+Login7+queries
              └── cleanup()
                    ├── clientConn.Close()
                    ├── backendConn.Close()
                    └── dq.Release(ctx, bucketID) ── EVALSHA release.lua + PUBLISH
```
