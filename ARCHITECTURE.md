# ARCHITECTURE.md — Connection Pooling Proxy para SQL Server

> **Propósito deste documento:** fornecer ao agente de IA contexto completo do projeto
> em uma única leitura, eliminando a necessidade de explorar arquivos um a um.
> Atualizar este doc a cada mudança significativa.
>
> **Última atualização:** 2026-02-18 (Fase 3 concluída)

---

## 1. Visão Geral

POC de um **proxy de connection pooling** que fica entre aplicações (.NET/Go/Python)
e instâncias **SQL Server 2022 (RDS)**, controlando o número de conexões simultâneas
de forma centralizada via **Redis** e expondo métricas via **Prometheus/Grafana**.

```
┌─────────────────────────────────────────────────────────────────────────┐
│                         Clients (sqlcmd, apps)                         │
└────────────────────────────────┬────────────────────────────────────────┘
                                 │ TDS :1433
                        ┌────────▼────────┐
                        │    HAProxy L4    │  leastconn
                        │  (simula NLB)    │  health: GET /health/live :8080
                        └──┬─────┬─────┬──┘
                           │     │     │
                 ┌─────────▼┐ ┌──▼────┐ ┌▼─────────┐
                 │ proxy-1  │ │proxy-2│ │ proxy-3   │  Go binary
                 │ :1433    │ │:1433  │ │ :1433     │  TDS relay
                 │ :8080    │ │:8080  │ │ :8080     │  health
                 │ :9090    │ │:9090  │ │ :9090     │  metrics
                 └──┬──┬────┘ └──┬────┘ └────┬──┬──┘
                    │  │         │            │  │
          ┌─────────┘  │    ┌────┘            │  └─────────┐
          │            │    │                 │            │
     ┌────▼────┐  ┌────▼────▼───┐  ┌─────────▼────┐  ┌───▼───┐
     │ SQL Srv │  │    Redis    │  │  SQL Srv     │  │SQL Srv│
     │ bucket-1│  │ coordinator │  │  bucket-2    │  │bkt-3  │
     │ :1433   │  │ :6379       │  │  :1433       │  │:1433  │
     └─────────┘  └─────────────┘  └──────────────┘  └───────┘
```

---

## 2. Stack Tecnológica

| Componente     | Tecnologia                        | Versão      |
|----------------|-----------------------------------|-------------|
| Linguagem      | Go                                | 1.24.0      |
| SQL Server     | Microsoft SQL Server 2022 (Linux) | 16.0.4236   |
| Redis          | Redis                             | 7.4.x       |
| Load Balancer  | HAProxy                           | 3.1         |
| Métricas       | Prometheus + Grafana              | —           |
| Driver SQL     | `github.com/microsoft/go-mssqldb` | 1.9.6       |
| Driver Redis   | `github.com/redis/go-redis/v9`    | 9.18.0      |
| Containers     | Docker Compose                    | —           |
| Build          | Multi-stage: `golang:1.24-alpine` → `alpine:3.19` |

---

## 3. Árvore de Diretórios

```
.
├── cmd/
│   ├── proxy/main.go              ← Entrypoint do proxy (176 loc)
│   │                                 Carrega config, inicia health/metrics/pool/coordinator/proxy
│   │                                 Graceful shutdown com SIGINT/SIGTERM
│   └── loadgen/main.go            ← Placeholder para load generator (14 loc)
│
├── internal/
│   ├── config/
│   │   └── config.go              ← Carrega proxy.yaml + buckets.yaml, valida, aplica defaults (219 loc)
│   │
│   ├── coordinator/               ← [FASE 3] Coordenação distribuída via Redis
│   │   ├── lua/
│   │   │   ├── acquire.lua        ← Script Lua atômico: GET count < max → INCR + HINCRBY (33 loc)
│   │   │   └── release.lua        ← Script Lua atômico: DECR + HINCRBY -1 + PUBLISH (38 loc)
│   │   ├── redis.go               ← RedisCoordinator: Acquire/Release/Subscribe/Fallback (479 loc)
│   │   ├── heartbeat.go           ← Heartbeat periódico + cleanup de instâncias mortas (196 loc)
│   │   └── semaphore.go           ← Semáforo distribuído: Pub/Sub + polling fallback (135 loc)
│   │
│   ├── health/
│   │   └── health.go              ← Health checker: Redis PING + SQL SELECT 1 por bucket (265 loc)
│   │                                 HTTP: /health, /health/ready, /health/live
│   │
│   ├── metrics/
│   │   └── metrics.go             ← Métricas Prometheus pré-registradas com promauto (92 loc)
│   │
│   ├── pool/                      ← [FASE 1] Connection pool local por bucket
│   │   ├── connection.go          ← PooledConn: wrapper sobre *sql.DB com state/pin/metadata (175 loc)
│   │   ├── health.go              ← Health check: PingContext nas idle connections (61 loc)
│   │   ├── manager.go             ← Manager: mapa de BucketPool, Acquire/Release/Discard (134 loc)
│   │   └── pool.go                ← BucketPool: LIFO idle stack, wait queue, eviction, min_idle (443 loc)
│   │
│   ├── proxy/                     ← [FASE 2] TDS proxy transparente
│   │   ├── handler.go             ← Session: Pre-Login relay → coordinator.Acquire → TCP relay (293 loc)
│   │   ├── listener.go            ← Server: TCP listener, accept loop, graceful shutdown (158 loc)
│   │   └── router.go              ← Router: Login7→bucket por database/serverName/username (136 loc)
│   │
│   ├── queue/                     ← [FASE 3] Fila distribuída
│   │   └── distributed.go         ← DistributedQueue: TryAcquire (fast) → Wait (slow) (116 loc)
│   │
│   └── tds/                       ← Parser mínimo do protocolo TDS (MS-TDS spec)
│       ├── packet.go              ← Header 8-byte, ReadPacket, ReadMessage, BuildPackets (266 loc)
│       ├── prelogin.go            ← Parse/Marshal Pre-Login, encryption options (209 loc)
│       ├── login7.go              ← Parse Login7: user, database, server, app (173 loc)
│       ├── pinning.go             ← Detecção de pinning: BEGIN TRAN, sp_prepare, ENVCHANGE (374 loc)
│       ├── relay.go               ← Relay bidirecional TDS, ForwardLogin7, DrainResponse (173 loc)
│       └── error.go               ← Construtor de TDS ERROR token para enviar ao client (179 loc)
│
├── pkg/
│   └── bucket/
│       └── bucket.go              ← Struct Bucket com DSN(), Addr() (49 loc)
│
├── configs/
│   ├── proxy.yaml                 ← Config do proxy: listen, redis, fallback
│   └── buckets.yaml               ← 3 buckets: bucket-001/002/003, max_connections=50
│
├── deployments/
│   ├── docker-compose.yml         ← 11 containers: 3 SQL Server, 3 proxy, Redis, HAProxy, Prometheus, Grafana, init-db
│   └── haproxy/
│       └── haproxy.cfg            ← L4 TCP leastconn, health check HTTP :8080
│
├── grafana/                       ← Dashboards e datasources provisionados
├── prometheus/
│   └── prometheus.yml             ← Scrape targets: proxy-1/2/3 :9090
├── scripts/
│   ├── init-databases.sql         ← CREATE DATABASE tenant_db
│   ├── seed-data.sql              ← Tabelas e dados de teste
│   ├── wait-for-sql.sh            ← Aguarda SQL Server ficar pronto
│   └── run-loadtest.sh            ← Script de teste de carga
│
├── postman/                       ← Collection Postman para testes manuais
├── Dockerfile                     ← Multi-stage build do proxy Go
├── go.mod / go.sum                ← Dependências Go
├── README.md
├── 01-ENTENDIMENTO-TECNICO.md     ← Documento de entendimento do problema
└── 02-PLANO-DE-EXECUCAO.md        ← Plano de execução com 8 fases
```

**Total:** ~4.515 linhas de Go em 23 arquivos.

---

## 4. Grafo de Dependência entre Pacotes

```
cmd/proxy/main.go
  ├── internal/config           ← carrega YAML
  ├── internal/health           ← checker HTTP
  ├── internal/metrics          ← Prometheus registry
  ├── internal/pool             ← pool manager
  ├── internal/coordinator      ← Redis coordinator + heartbeat
  └── internal/proxy            ← TDS proxy server
        ├── internal/config
        ├── internal/coordinator  ← coordinator.Acquire/Release por sessão
        ├── internal/metrics
        ├── internal/pool
        ├── internal/tds          ← packet parsing, pre-login, login7, relay
        └── pkg/bucket

internal/coordinator
  ├── internal/config
  ├── internal/metrics
  └── github.com/redis/go-redis/v9

internal/pool
  ├── internal/metrics
  ├── pkg/bucket
  └── github.com/microsoft/go-mssqldb

internal/queue
  ├── internal/coordinator
  └── internal/metrics

internal/tds
  └── (nenhuma dependência interna)

pkg/bucket
  └── (nenhuma dependência)
```

**Regra:** `pkg/` é importável por qualquer pacote. `internal/` respeita visibilidade Go.
`tds` e `bucket` não dependem de nada interno (folhas do grafo).

---

## 5. Fluxo de uma Requisição SQL (End-to-End)

```
Client                HAProxy           Proxy (Go)              Redis           SQL Server
  │                      │                   │                    │                  │
  ├─ TDS PRELOGIN ──────►│                   │                    │                  │
  │                      ├─ TCP leastconn ──►│                    │                  │
  │                      │                   ├─ parse PreLogin    │                  │
  │                      │                   ├─ pickBucket()      │                  │
  │                      │                   │  (bucket-001)      │                  │
  │                      │                   │                    │                  │
  │                      │                   ├─ coordinator       │                  │
  │                      │                   │  .Acquire(bucket)──►│ EVALSHA acquire  │
  │                      │                   │                    ├─ INCR count      │
  │                      │                   │◄──── slot ok ──────┤  HINCRBY inst    │
  │                      │                   │                    │                  │
  │                      │                   ├─ net.Dial ────────────────────────────►│
  │                      │                   ├─ forward PreLogin ───────────────────►│
  │                      │                   │◄──────────────── PreLogin Response ───┤
  │◄───────────────── PreLogin Response ─────┤                    │                  │
  │                      │                   │                    │                  │
  │═══════════ TLS Handshake (transparente via io.Copy) ═══════════════════════════│
  │═══════════ Login7 (encrypted, relayed transparently) ═════════════════════════│
  │═══════════ Login Response (encrypted, relayed) ═══════════════════════════════│
  │                      │                   │                    │                  │
  │── SQL_BATCH ────────────────────────────►├── io.Copy ────────────────────────────►│
  │                      │                   │                    │                  │
  │◄──────────────────── REPLY ──────────────┤◄── io.Copy ───────────────────────────┤
  │                      │                   │                    │                  │
  │── TCP FIN ──────────────────────────────►│                    │                  │
  │                      │                   ├─ cleanup()         │                  │
  │                      │                   ├─ coordinator       │                  │
  │                      │                   │  .Release(bucket)──►│ EVALSHA release  │
  │                      │                   │                    ├─ DECR count      │
  │                      │                   │                    ├─ PUBLISH notify  │
  │                      │                   ├─ close backend ─────────────────── ×──┤
```

### Detalhe importante sobre o fluxo atual

O proxy opera em modo **relay TCP transparente** após o Pre-Login:
- **NÃO** faz parsing TDS durante TLS (tudo opaco via `io.Copy`)
- **NÃO** faz routing por Login7 (o bucket é escolhido antes, no Pre-Login)
- **NÃO** usa o pool de conexões `*sql.DB` para o tráfego TDS (usa `net.Dial` direto)
- O pool `*sql.DB` existe para health checks e operações internas (`sp_reset_connection`)
- O Router (Login7-based) está implementado mas **não é usado** no fluxo atual
- A detecção de pinning (`tds/pinning.go`) está implementada mas **não é ativada** durante TCP relay

---

## 6. Componentes Principais — Resumo de Responsabilidades

### 6.1 `cmd/proxy/main.go`
**Orquestrador de startup/shutdown.** Sequência:
1. `config.Load()` → proxy.yaml + buckets.yaml
2. Métricas HTTP `:9090/metrics`
3. Health checker HTTP `:8080/health`
4. `pool.NewManager()` → 3 BucketPools (5 idle cada)
5. `coordinator.NewRedisCoordinator()` → Redis connect, Lua scripts, instance registration
6. `coordinator.NewHeartbeat().Start()` → heartbeat periódico
7. `proxy.NewServer().Start()` → TCP listener `:1433`
8. Aguarda SIGINT/SIGTERM → shutdown reverso

### 6.2 `internal/proxy` — TDS Proxy
- **Server** (`listener.go`): TCP accept loop, spawna `Session` por conexão
- **Session** (`handler.go`): Lifecycle completo de uma sessão TDS
  - Lê Pre-Login → escolhe bucket → `coordinator.Acquire` → `net.Dial` backend
  - Forward Pre-Login → relay TCP bidirecional → cleanup + `coordinator.Release`
- **Router** (`router.go`): Resolve Login7 → bucket (por server name / database / username)
  - *Implementado mas não ativado no fluxo atual (bucket escolhido antes do Login7)*

### 6.3 `internal/coordinator` — Coordenação Distribuída
- **RedisCoordinator** (`redis.go`):
  - `Acquire(ctx, bucketID)` → EvalSha acquire.lua → fallback local se Redis falhar
  - `Release(ctx, bucketID)` → EvalSha release.lua → PUBLISH para Pub/Sub
  - `Subscribe(ctx, bucketID)` → canal de notificação de releases
  - Fallback mode: `enterFallback()` / `ExitFallback()` com reconciliação
- **Heartbeat** (`heartbeat.go`):
  - Envia `SET key TTL` a cada 10s
  - A cada 30s: detecta instâncias mortas (sem heartbeat) e limpa seus contadores
- **Semaphore** (`semaphore.go`):
  - `Wait(ctx, bucketID, timeout)` → Pub/Sub + polling para esperar slot
  - `TryAcquire(ctx, bucketID)` → tentativa não-bloqueante

### 6.4 `internal/pool` — Connection Pool Local
- **Manager** (`manager.go`): Mapa `bucketID → BucketPool`, Acquire/Release/Discard
- **BucketPool** (`pool.go`):
  - Idle stack LIFO, wait queue (channel-based), `sp_reset_connection` no release
  - Maintenance loop (30s): evict stale, ensure min_idle
- **PooledConn** (`connection.go`): Wrapper sobre `*sql.DB` com state, pin, use count
- **HealthCheck** (`health.go`): `PingContext` em idle connections

### 6.5 `internal/tds` — Parser TDS Mínimo
- **packet.go**: Header 8-byte, ReadPacket/ReadMessage/BuildPackets/WritePackets
- **prelogin.go**: Parse/Marshal Pre-Login, encryption flags
- **login7.go**: Parse Login7 (offset/length pairs, UTF-16 LE)
- **pinning.go**: Detecta BEGIN TRAN, sp_prepare, BULK_LOAD, ENVCHANGE tokens
- **relay.go**: Relay bidirecional, RelayMessage, ForwardLogin7, DrainResponse
- **error.go**: Constrói TDS ERROR token (50001 pool exhausted, 50002 routing, 50003 backend)

### 6.6 `internal/queue` — Fila Distribuída
- **DistributedQueue** (`distributed.go`): Wrapper que combina Semaphore + Coordinator
  - Fast path: `TryAcquire` → slow path: `semaphore.Wait` com timeout

---

## 7. Redis — Chaves e Padrões

| Chave                               | Tipo      | TTL  | Descrição                                  |
|--------------------------------------|-----------|------|--------------------------------------------|
| `proxy:bucket:{id}:count`           | String    | ∞    | Contagem global de conexões ativas         |
| `proxy:bucket:{id}:max`             | String    | ∞    | Limite máximo de conexões                  |
| `proxy:instance:{id}:conns`         | Hash      | ∞    | `{ bucket_id: local_count }` por instância |
| `proxy:instance:{id}:heartbeat`     | String    | 30s  | Timestamp, expira se instância morrer      |
| `proxy:instances`                    | Set       | ∞    | IDs de instâncias ativas                   |
| `proxy:release:{bucket_id}` (canal) | Pub/Sub   | —    | Notificação quando conexão é liberada      |

### Scripts Lua (executados via EVALSHA)

**acquire.lua** — 3 KEYS: `count`, `max`, `instance_hash`
- `GET count` < `GET max` → `INCR count` + `HINCRBY instance bucket 1`
- Retorna: `>0` (sucesso), `-1` (lotado), `-2` (max não configurado)

**release.lua** — 2 KEYS: `count`, `instance_hash` + ARGV: `bucket_id`, `channel`
- Proteção contra underflow (count ≤ 0 → SET 0)
- `DECR count` + `HINCRBY instance bucket -1` + `PUBLISH channel bucket_id`
- Retorna: novo count

---

## 8. Infraestrutura Docker

| Container          | Imagem                         | Portas (host)     | Descrição                 |
|--------------------|--------------------------------|-------------------|---------------------------|
| sqlserver-bucket-1 | mcr.microsoft.com/mssql/server | 14331:1433        | SQL Server bucket-001     |
| sqlserver-bucket-2 | mcr.microsoft.com/mssql/server | 14332:1433        | SQL Server bucket-002     |
| sqlserver-bucket-3 | mcr.microsoft.com/mssql/server | 14333:1433        | SQL Server bucket-003     |
| redis              | redis:7-alpine                 | 6379:6379         | Coordenação distribuída   |
| proxy-1            | Dockerfile (Go multi-stage)    | 11433:1433, 18081:8080, 19091:9090 | Proxy instância 1 |
| proxy-2            | Dockerfile                     | 11434:1433, 18082:8080, 19092:9090 | Proxy instância 2 |
| proxy-3            | Dockerfile                     | 11435:1433, 18083:8080, 19093:9090 | Proxy instância 3 |
| haproxy            | haproxy:3.1                    | 1433:1433, 8404:8404 | L4 TCP leastconn       |
| prometheus         | prom/prometheus                | 9090:9090         | Coleta métricas           |
| grafana            | grafana/grafana-oss            | 3000:3000         | Dashboards                |
| init-db            | mcr.microsoft.com/mssql-tools  | —                 | Seed databases (run-once) |

**Rede:** `proxy-network` (bridge), todos os containers na mesma rede.

---

## 9. Métricas Prometheus

| Métrica                           | Tipo      | Labels                        |
|-----------------------------------|-----------|-------------------------------|
| `proxy_connections_active`        | Gauge     | `bucket_id`                   |
| `proxy_connections_idle`          | Gauge     | `bucket_id`                   |
| `proxy_connections_pinned`        | Gauge     | `bucket_id`, `pin_reason`     |
| `proxy_connections_max`           | Gauge     | `bucket_id`                   |
| `proxy_connections_total`         | Counter   | `bucket_id`, `status`         |
| `proxy_queue_length`              | Gauge     | `bucket_id`                   |
| `proxy_queue_wait_seconds`        | Histogram | `bucket_id`                   |
| `proxy_tds_packets_total`         | Counter   | `bucket_id`, `direction`, `type` |
| `proxy_query_duration_seconds`    | Histogram | `bucket_id`                   |
| `proxy_connection_errors_total`   | Counter   | `bucket_id`, `error_type`     |
| `proxy_redis_operations_total`    | Counter   | `operation`, `status`         |
| `proxy_instance_heartbeat`        | Gauge     | `instance_id`                 |
| `proxy_pinning_duration_seconds`  | Histogram | `bucket_id`, `pin_reason`     |

---

## 10. Configuração

### proxy.yaml (valores default)
```yaml
proxy:
  listen_addr: "0.0.0.0"    listen_port: 1433
  session_timeout: 5m        idle_timeout: 60s
  queue_timeout: 30s         pinning_mode: "transaction"
  health_check_port: 8080    metrics_port: 9090

redis:
  addr: "redis:6379"         pool_size: 20
  heartbeat_interval: 10s    heartbeat_ttl: 30s

fallback:
  enabled: true              local_limit_divisor: 3   # 50/3 ≈ 16 conn/instance
```

### buckets.yaml (3 buckets idênticos)
```yaml
buckets:
  - id: bucket-001/002/003   host: sqlserver-bucket-1/2/3
    port: 1433               database: tenant_db
    max_connections: 50       min_idle: 5
    max_idle_time: 300s       connection_timeout: 30s
```

---

## 11. Status das Fases

| Fase | Nome                            | Status | Notas                                                  |
|------|---------------------------------|--------|--------------------------------------------------------|
| 0    | Infraestrutura Docker           | ✅     | 11 containers, tudo funcional                          |
| 1    | Pool Manager Local              | ✅     | BucketPool com LIFO, wait queue, min_idle, eviction    |
| 2    | TDS Wire Protocol Proxy         | ✅     | Relay TCP transparente após Pre-Login                  |
| 3    | Coordenação Distribuída (Redis) | ✅     | Lua scripts, heartbeat, semáforo, fallback mode        |
| 4    | Session Pinning                 | 🔲     | Detecção implementada, integração pendente             |
| 5    | Métricas e Observabilidade      | 🔲     | Métricas registradas, dashboards a completar           |
| 6    | Testes de Carga                 | 🔲     |                                                        |
| 7    | Documentação Final              | 🔲     |                                                        |

### Pontos de atenção para a próxima fase (4)
1. **O proxy NÃO faz parsing TDS após Pre-Login** — usa `io.Copy` transparente
   - Para habilitar pinning, precisa trocar `io.Copy` por `tds.Relay` (apenas em modo sem TLS)
   - Ou implementar TLS termination no proxy para poder inspecionar pacotes
2. **O pool `*sql.DB` NÃO é usado para tráfego TDS** — conexões são `net.Conn` diretas
   - O pool serve para health checks internos e `sp_reset_connection`
3. **O Router por Login7 está implementado mas não ativado** — bucket é escolhido via `pickBucket()` (primeiro bucket)
4. `pinning.go` tem implementação completa (InspectPacket, InspectResponse, ENVCHANGE parsing) — falta integração

---

## 12. Comandos Úteis

```bash
# Build
go build ./...
go vet ./...

# Deploy
docker compose -f deployments/docker-compose.yml up -d --build proxy-1 proxy-2 proxy-3

# Teste rápido
docker exec sqlserver-bucket-1 /opt/mssql-tools18/bin/sqlcmd \
  -S host.docker.internal,1433 -U sa -P 'YourStr0ngP@ssword1' -C \
  -Q "SELECT @@SERVERNAME, GETDATE()"

# Redis
docker exec redis redis-cli SMEMBERS proxy:instances
docker exec redis redis-cli GET proxy:bucket:bucket-001:count
docker exec redis redis-cli HGETALL proxy:instance:<id>:conns

# Métricas
curl -s http://localhost:19091/metrics | grep proxy_redis

# Logs
docker logs proxy-1 2>&1 | grep coordinator
```
