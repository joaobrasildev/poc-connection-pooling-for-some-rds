# PHASE-STATUS.md — Estado de Execução por Fase

> **Propósito:** o agente lê este arquivo primeiro e sabe exatamente
> **onde parou**, **o que está feito**, **o que falta**, e **qual é o próximo passo**.
>
> **Regra:** ao concluir uma fase, o agente DEVE atualizar este arquivo
> antes de encerrar a sessão.
>
> **Última atualização:** 2026-02-18 (Fase 4 concluída)

---

## Resumo Rápido

| Fase | Nome                              | Status         | Observação |
|------|-----------------------------------|----------------|------------|
| 0    | Infraestrutura Docker             | ✅ Concluída   | 11 containers funcionando |
| 1    | Connection Pool Manager           | ✅ Concluída   | BucketPool com LIFO, wait queue, min_idle, eviction |
| 2    | TDS Wire Protocol Proxy           | ✅ Concluída   | TCP relay transparente (io.Copy) |
| 3    | Coordenação Distribuída (Redis)   | ✅ Concluída   | Lua scripts, heartbeat, semaphore, fallback |
| 4    | Fila de Espera e Backpressure     | ✅ Concluída   | Circuit breaker, erros tipados, integração completa |
| 5    | Observabilidade                   | 🔲 Não iniciada | Próxima fase |
| 6    | Load Generator e Testes de Carga  | 🔲 Não iniciada | |
| 7    | Hardening e Documentação          | 🔲 Não iniciada | |

---

## Fase 0 — Infraestrutura Docker ✅

**Concluída em:** sessão de implementação inicial

### O que foi entregue
- `docker-compose.yml` com 11 containers
- 3 SQL Server 2022 (bucket-001/002/003), cada um com `tenant_db`
- Redis 7.4.x standalone
- HAProxy 3.1 (L4 TCP, leastconn, health check HTTP GET :8080)
- Prometheus + Grafana (dashboards não customizados ainda)
- Init-db container (seed T-SQL)
- 3 instâncias do proxy (proxy-1/2/3)

### Arquivos-chave
| Arquivo | Conteúdo |
|---------|----------|
| `deployments/docker-compose.yml` | Todos os 11 services |
| `deployments/haproxy/haproxy.cfg` | L4 TCP balancing entre proxy-1/2/3 |
| `configs/proxy.yaml` | Config do proxy (ports, timeouts, redis, buckets ref) |
| `configs/buckets.yaml` | 3 buckets com host/port/max_connections |
| `prometheus/prometheus.yml` | Scrape config para proxy-1/2/3 |
| `Dockerfile` | Multi-stage: golang:1.24-alpine → alpine:3.19 |

### Validação feita
- `docker compose up` sobe todos os containers
- `sqlcmd` conecta em cada SQL Server via proxy
- Prometheus scrape funcionando

### Diferenças vs Plano Original
- 3 buckets em vez de 5 (economia de RAM — cada SQL Server usa ~2GB)
- HAProxy 3.1 em vez de 2.9

---

## Fase 1 — Connection Pool Manager ✅

**Concluída em:** sessão de implementação da Fase 1

### O que foi entregue
- `BucketPool` com LIFO idle stack, wait queue com timeout, eviction, min_idle
- Ciclo: Acquire → Use → Release → sp_reset_connection → return to pool
- Health check periódico (PingContext / SELECT 1)
- Config loader (proxy.yaml + buckets.yaml)

### Arquivos-chave
| Arquivo | LOC | Conteúdo |
|---------|-----|----------|
| `internal/pool/pool.go` | 443 | BucketPool (Acquire/Release/evict/refill) |
| `internal/pool/connection.go` | 175 | PooledConn wrapper com state/pin tracking |
| `internal/pool/health.go` | 61 | PingContext health checker |
| `internal/pool/manager.go` | 134 | Manager orquestrando N pools |
| `internal/config/config.go` | ~100 | YAML loader |
| `pkg/bucket/bucket.go` | 50 | Bucket struct |

### ⚠️ Ponto importante para fases futuras
O `BucketPool` gerencia conexões `*sql.DB` — que **não são usadas para tráfego
TDS** (ver ADR-003 em DECISIONS.md). O controle de limites para sessões TDS
é feito pelo `coordinator.Acquire/Release` (Fase 3).

---

## Fase 2 — TDS Wire Protocol Proxy ✅

**Concluída em:** sessão de implementação da Fase 2

### O que foi entregue
- TCP listener na porta 1433
- Pre-Login: proxy lê, faz forward ao backend, devolve resposta ao client
- Após Pre-Login: relay TCP transparente (`io.Copy` bidirecional)
- Login7 parser implementado (extrai database, username, server_name)
- Router por database name implementado (mas **não ativado** — ver ADR-004)
- Pinning detector implementado (InspectPacket/InspectResponse — mas **não ativado** — ver ADR-001)
- Relay com PacketCallback implementado (mas substituído por `io.Copy` — ver ADR-001)
- TDS Error builder (envia erro TDS ao client em caso de falha)

### Arquivos-chave
| Arquivo | LOC | Conteúdo |
|---------|-----|----------|
| `internal/proxy/handler.go` | 293 | Session handler (Pre-Login + io.Copy relay) |
| `internal/proxy/listener.go` | 158 | TCP listener + accept loop |
| `internal/proxy/router.go` | 136 | Router por Login7 database (NÃO ATIVADO) |
| `internal/tds/packet.go` | 266 | TDS header/packet parsing |
| `internal/tds/prelogin.go` | 209 | Pre-Login parse + marshal |
| `internal/tds/login7.go` | 173 | Login7 parser |
| `internal/tds/pinning.go` | 374 | Pin detection (NÃO ATIVADO) |
| `internal/tds/relay.go` | 173 | Relay com callback (NÃO USADO — io.Copy em vez) |
| `internal/tds/error.go` | 179 | TDS ERROR token builder |

### ⚠️ Código implementado mas NÃO ativado (impacta Fase 4)
1. **`router.go`** — está pronto, seria ativado mudando `pickBucket()` em handler.go
2. **`pinning.go`** — InspectPacket/InspectResponse completos, precisam de integração
3. **`relay.go`** — Relay com PacketCallback existe, mas handler.go usa `io.Copy`

### Validação feita
- `sqlcmd -S localhost,1433 -U sa -P ... -d tenant_db` funciona via proxy
- SELECT, INSERT, UPDATE, transactions, stored procedures — tudo funcional
- Conexão TLS end-to-end entre client e backend (proxy não interfere)

---

## Fase 3 — Coordenação Distribuída (Redis) ✅

**Concluída em:** sessão de implementação da Fase 3

### O que foi entregue
- `RedisCoordinator` com Acquire/Release atômicos via Lua EVALSHA
- Scripts Lua embeddados via `//go:embed`
- Heartbeat periódico (SET com TTL=30s, intervalo 10s)
- Cleanup de instâncias mortas (detecta heartbeat expirado, corrige contadores)
- Semáforo distribuído (Pub/Sub + polling para wait quando bucket está lotado)
- Fallback mode (Redis indisponível → limite local com divisor)
- Reconciliação ao sair de fallback
- Integração no handler.go: Acquire antes de conectar ao backend, Release no cleanup

### Arquivos-chave
| Arquivo | LOC | Conteúdo |
|---------|-----|----------|
| `internal/coordinator/redis.go` | 479 | RedisCoordinator (Acquire/Release/Fallback/reconcile) |
| `internal/coordinator/heartbeat.go` | 196 | Heartbeat + cleanupDeadInstances |
| `internal/coordinator/semaphore.go` | 135 | Semáforo distribuído (Pub/Sub + poll) |
| `internal/coordinator/lua/acquire.lua` | 33 | Atomic check-and-increment |
| `internal/coordinator/lua/release.lua` | 38 | Atomic decrement + PUBLISH |
| `internal/queue/distributed.go` | 116 | DistributedQueue (Semaphore + Coordinator) |

### Validação feita
- 3 proxies respeitando limite global de 50 conexões por bucket
- Kill de um proxy → heartbeat detecta → cleanup em ~30s → capacidade recuperada
- Redis down → fallback mode → Redis up → reconciliação automática

---

## Fase 4 — Fila de Espera e Backpressure ✅

**Concluída em:** 2026-02-18 
**ADR:** ADR-007 (reutilizar DistributedQueue da Fase 3)

### O que foi entregue
- `DistributedQueue` evoluída com circuit breaker (`maxQueueSize`)
- `QueueError` tipado com `IsQueueFull()` / `IsQueueTimeout()`
- `ErrQueueTimeout` (50004) e `ErrQueueFull` (50005) em `tds/error.go`
- `handler.go` agora usa `dqueue.Acquire()` em vez de `coordinator.Acquire()` direto
- Config `max_queue_size` (default: 1000) adicionada a `ProxyConfig` e `proxy.yaml`
- Pipeline completo: `main.go` → `NewDistributedQueue()` → `Server` → `Session`

### Arquivos modificados
| Arquivo | Mudança |
|---------|--------|
| `internal/queue/distributed.go` | +circuit breaker, +QueueError, +IsQueueFull/IsQueueTimeout, assinatura NewDistributedQueue mudou |
| `internal/tds/error.go` | +ErrQueueTimeout (50004), +ErrQueueFull (50005) |
| `internal/proxy/handler.go` | +dqueue field, usa dqueue.Acquire com erros tipados |
| `internal/proxy/listener.go` | +dqueue field, NewServer recebe dqueue |
| `cmd/proxy/main.go` | +cria DistributedQueue, passa ao NewServer |
| `internal/config/config.go` | +MaxQueueSize no ProxyConfig + default |
| `configs/proxy.yaml` | +max_queue_size: 1000 |

### Fluxo de aquisição (atualizado)
```
Client conecta → Pre-Login → pickBucket()
       ↓
dqueue.Acquire(bucketID)
       ↓ fast path ok?
  [sim] → slot adquirido → dial backend → relay
  [não] → fila cheia (circuit breaker)?
            [sim] → TDS Error 50005 (ErrQueueFull)
            [não] → Semaphore.Wait (Pub/Sub + polling)
                      ↓ timeout?
                  [sim] → TDS Error 50004 (ErrQueueTimeout)
                  [não] → slot adquirido → dial backend → relay
```

### Métricas populadas nesta fase
| Métrica | Labels | Status |
|---------|--------|--------|
| `proxy_queue_length` | `bucket_id` | ✅ Populada (incrementDepth/decrementDepth) |
| `proxy_queue_wait_seconds` | `bucket_id` | ✅ Populada (Semaphore.Wait) |
| `proxy_connections_total` | `bucket_id`, `status` | ✅ Novos status: `acquired`, `acquired_after_wait`, `timeout`, `cancelled`, `rejected_queue_full` |
| `proxy_connection_errors_total` | `bucket_id`, `error_type` | ✅ Novos tipos: `queue_full`, `queue_timeout` |

### Validação feita

#### 1. Build
- `go build ./...` — compila sem erros ✅

#### 2. Infraestrutura
- `docker compose up -d --build` — 11 containers sobem healthy ✅
- init-db: "All databases initialized and seeded!" ✅

#### 3. Smoke tests
| Teste | Resultado |
|-------|-----------|
| `SELECT 1` via proxy | ✅ OK |
| `INSERT` + `SELECT` via proxy | ✅ OK (tenant test-phase4, id=101) |
| `BEGIN TRAN` + `INSERT` + `COMMIT` via proxy | ✅ OK (order ORD-PHASE4-001) |
| Stored procedure `sp_connection_info` via HAProxy | ✅ OK |
| 10 conexões concorrentes (script Go) | ✅ 10/10 OK |
| 20 holders + 5 extras (fila de espera) | ✅ 25/25 OK |

#### 4. Queue Timeout (TDS Error 50004)
- **Setup:** `max_connections` reduzido para 3 no Redis, 3 holders saturando o bucket
- **Comportamento:** conexão extra entra na fila, aguarda 30s, recebe timeout
- **Log confirmado:** `[dqueue] Wait timed out for bucket bucket-001 after 30.006490348s`
- **Log confirmado:** `[session:25] Queue acquire failed for bucket bucket-001: queue timeout`
- **Métrica:** `proxy_connection_errors_total{error_type="queue_timeout"} 1` ✅
- **Métrica:** `proxy_connections_total{status="timeout"} 1` ✅
- **Tempo:** ~30s (consistente com `queue_timeout: 30s`) ✅

#### 5. Circuit Breaker (TDS Error 50005)
- **Setup:** `max_queue_size=2`, `queue_timeout=10s`, `max_connections=3` (Redis)
- **Carga:** 3 holders + 10 conexões extras em paralelo via HAProxy
- **Comportamento:** 4 conexões rejeitadas instantaneamente (~18ms) pelo circuit breaker
- **Log confirmado:** `[dqueue] Circuit breaker: rejecting request for bucket bucket-001 (queue depth=2, max=2)` (4 ocorrências)
- **Métrica:** `proxy_connection_errors_total{error_type="queue_full"} 4` (total entre 3 proxies) ✅
- **Métrica:** `proxy_connections_total{status="rejected_queue_full"} 4` (total entre 3 proxies) ✅

#### 6. Métricas Prometheus (`/metrics`)
| Métrica | Valor verificado | Status |
|---------|-----------------|--------|
| `proxy_connections_total{status="acquired"}` | 41+ (distribuído entre 3 proxies) | ✅ |
| `proxy_connections_total{status="acquired_after_wait"}` | 1 | ✅ |
| `proxy_connections_total{status="timeout"}` | 1 | ✅ |
| `proxy_connections_total{status="rejected_queue_full"}` | 4 | ✅ |
| `proxy_queue_length{bucket_id="bucket-001"}` | 0 (correto, fila vazia) | ✅ |
| `proxy_queue_wait_seconds_sum` | 11.009s | ✅ |
| `proxy_connection_errors_total{error_type="queue_timeout"}` | 9+ | ✅ |
| `proxy_connection_errors_total{error_type="queue_full"}` | 4 | ✅ |

#### 7. Health Check
- `GET /health/live` em todos os 3 proxies → `{"status":"alive"}` ✅

#### 8. Logs
- Nenhum panic, fatal, ou erro inesperado nos logs dos 3 proxies ✅
- Todos os logs de circuit breaker, timeout e acquired_after_wait confirmados ✅

---

## Fase 5 — Observabilidade 🔲

**Status:** Não iniciada \
**Referência:** `02-PLANO-DE-EXECUCAO.md` seção "Fase 5"

### Escopo resumido
- Dashboard Grafana customizado (7 painéis definidos no plano)
- Popular métricas que existem mas estão vazias (ver CONTRACTS.md seção 10)
- Alerting rules (queue > threshold, errors > threshold)

### Métricas registradas mas NÃO populadas (a resolver)
- `TDSPacketsTotal` — requer relay com parsing (depende de ADR-001)
- `QueryDuration` — idem
- `PinningDuration` — requer pinning ativo (depende de ADR-001)

---

## Fase 6 — Load Generator e Testes de Carga 🔲

**Status:** Não iniciada \
**Referência:** `02-PLANO-DE-EXECUCAO.md` seção "Fase 6"

### Escopo resumido
- Load generator em Go usando `go-mssqldb` como driver
- 9 cenários de teste definidos (baseline, burst, steady state, instance failure,
  redis failure, uneven distribution, scale out, long transactions, prepared storm)
- Query mix: 60% simples, 15% prepared, 15% transactions, 5% sprocs, 5% DDL

### Dependências
- Fase 4 (fila precisa funcionar para cenários de burst)
- Fase 5 (métricas precisam estar populadas para validar resultados)

---

## Fase 7 — Hardening e Documentação 🔲

**Status:** Não iniciada \
**Referência:** `02-PLANO-DE-EXECUCAO.md` seção "Fase 7"

### Escopo resumido
- TLS termination (resolveria ADR-001 se necessário)
- Graceful shutdown (drain connections) — ⚠️ parcialmente implementado em `cmd/proxy/main.go`
- Connection leak detector
- Retry automático em falhas transientes
- Rate limiting por tenant
- README.md completo, diagramas, runbook

---

## Checklist do Agente ao Iniciar uma Sessão

```
1. Ler PHASE-STATUS.md              → saber onde parou
2. Ler ARCHITECTURE.md              → entender topologia e fluxo
3. Ler CONTRACTS.md (se necessário)  → consultar assinaturas sem abrir .go
4. Ler DECISIONS.md (se necessário)  → não refazer decisões já tomadas
5. Ler a seção da fase no 02-PLANO-DE-EXECUCAO.md → requisitos detalhados
6. Iniciar implementação
7. Ao concluir → ATUALIZAR ESTE ARQUIVO antes de encerrar
```

---

## Histórico de Atualizações

| Data | Fase | Ação |
|------|------|------|
| 2026-02-18 | 0-3 | Documento criado com estado retroativo das fases 0-3 |
| 2026-02-18 | 4   | Fase 4 concluída — circuit breaker, erros tipados, integração dqueue (ADR-007) |
| 2026-02-18 | 4   | Validação E2E completa: smoke tests, queue timeout (50004), circuit breaker (50005), métricas, health, logs |
