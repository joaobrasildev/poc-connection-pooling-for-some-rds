# Plano de Execução — POC Connection Pooling Proxy para SQL Server

## Sumário Executivo

Este documento detalha o plano completo de implementação da POC de um **Proxy de Connection Pooling em Go** para **RDS SQL Server**, incluindo fases, entregáveis, decisões técnicas, arquitetura da simulação e análise de viabilidade.

### Mudanças em relação à versão anterior (pós-validação)

- **Engine definido:** SQL Server (protocolo TDS)
- **Zero mudança na coreVM:** Proxy implementa TDS transparente
- **Prepared statements + DDL + Stored Procedures:** Connection pinning obrigatório
- **Docker:** Containers `mcr.microsoft.com/mssql/server:2022-latest` no lugar de MySQL/PostgreSQL
- **Referências atualizadas:** Removidas referências a ProxySQL/PgBouncer como modelo direto

---

## Fase 0 — Setup do Projeto e Infraestrutura de Simulação

**Objetivo:** Criar toda a base do projeto Go e a infraestrutura Docker que simula o ambiente real com SQL Server.

### 0.1 Estrutura do Projeto Go

```
poc-connection-pooling-for-some-rds/
├── cmd/
│   ├── proxy/              # Entrypoint do proxy
│   │   └── main.go
│   └── loadgen/            # Gerador de carga (simula coreVMs)
│       └── main.go
├── internal/
│   ├── config/             # Configuração (YAML/ENV)
│   ├── proxy/              # Core do proxy (listener, handler, router)
│   ├── pool/               # Connection pool manager
│   ├── coordinator/        # Coordenação distribuída (Redis)
│   ├── queue/              # Fila de espera por bucket
│   ├── metrics/            # Prometheus metrics
│   ├── health/             # Health checks
│   └── tds/                # TDS protocol parser (handshake, packet relay, pinning detection)
├── pkg/
│   └── bucket/             # Modelo de bucket e configurações
├── deployments/
│   ├── docker-compose.yml  # Ambiente completo
│   ├── docker-compose.monitoring.yml
│   └── haproxy/
│       └── haproxy.cfg     # Config do Load Balancer L4
├── configs/
│   ├── proxy.yaml          # Config do proxy
│   └── buckets.yaml        # Mapeamento bucket → SQL Server
├── scripts/
│   ├── init-databases.sql  # Script T-SQL para criar databases nos containers
│   ├── seed-data.sql       # Seed de dados para teste (T-SQL)
│   └── run-loadtest.sh     # Executa teste de carga
├── grafana/
│   └── dashboards/
│       └── proxy-dashboard.json
├── prometheus/
│   └── prometheus.yml
├── Dockerfile
├── Dockerfile.loadgen
├── Makefile
├── go.mod
├── go.sum
└── README.md
```

### 0.2 Infraestrutura Docker (Simulação dos Buckets/RDS SQL Server)

**Não será utilizado LocalStack.** O que importa para o proxy é o **protocolo TDS**, que é idêntico entre um SQL Server em container Docker e um RDS SQL Server na AWS. A simulação via Docker é mais leve, rápida e fiel ao cenário real.

#### Componentes do Docker Compose

| Serviço | Imagem | Quantidade | Função |
|---|---|---|---|
| `sqlserver-bucket-1..N` | `mcr.microsoft.com/mssql/server:2022-latest` | 5 (simulando 5 buckets) | Simular RDS SQL Server instances |
| `redis` | `redis:7-alpine` | 1 (ou 3 para cluster) | Estado compartilhado |
| `proxy-1..N` | Build local (Go) | 3 instâncias | Proxy com scaling horizontal |
| `haproxy` | `haproxy:2.9` | 1 | Load Balancer L4 (simula NLB) |
| `prometheus` | `prom/prometheus` | 1 | Coleta de métricas |
| `grafana` | `grafana/grafana` | 1 | Dashboards |
| `loadgen` | Build local (Go) | 1-N | Simular coreVMs |

> **Nota sobre SQL Server em Docker:** A imagem oficial da Microsoft (`mcr.microsoft.com/mssql/server:2022-latest`) roda SQL Server no Linux e requer no mínimo **2GB de RAM por container**. Para a POC com 5 buckets, são necessários **~10GB de RAM** na máquina de desenvolvimento. Se necessário, podemos reduzir para 3 buckets.

> **Requisito de licença:** A imagem Docker usa a edição **Developer** (gratuita para dev/test). Definido via variável de ambiente `MSSQL_PID=Developer`.

### 0.3 Entregáveis da Fase 0

- [ ] Projeto Go inicializado com `go mod init`
- [ ] `docker-compose.yml` com SQL Server containers, Redis, HAProxy, Prometheus, Grafana
- [ ] Script T-SQL de inicialização dos databases (`init-databases.sql`)
- [ ] Makefile com targets: `build`, `run`, `test`, `docker-up`, `docker-down`
- [ ] Health check básico respondendo em cada serviço
- [ ] Verificação de conectividade: conectar via `sqlcmd` em cada container SQL Server

**Estimativa:** 1-2 dias

---

## Fase 1 — Connection Pool Manager (Single Instance)

**Objetivo:** Implementar o core do pool de conexões para uma única instância do proxy, sem coordenação distribuída.

### 1.1 Tarefas

1. **Config loader** — Carregar configuração de buckets a partir de YAML:
   ```yaml
   proxy:
     listen_port: 1433
     session_timeout: 5m          # Tempo máximo de sessão padrão
     idle_timeout: 60s            # Timeout para conexões idle
     queue_timeout: 30s           # Tempo máximo de espera na fila
     pinning_mode: "transaction"  # transaction | session

   buckets:
     - id: "bucket-001"
       host: "sqlserver-bucket-1"
       port: 1433
       database: "tenant_db"
       max_connections: 50
       min_idle: 5
       max_idle_time: 300s
       connection_timeout: 30s
   ```

2. **Pool por Bucket** — Para cada bucket, manter um pool de conexões SQL Server:
   - Conexões mínimas idle (warm pool) — conectadas ao SQL Server backend via `go-mssqldb`
   - Conexões máximas ativas
   - Health check periódico das conexões idle (via `SELECT 1`)
   - Eviction de conexões idle antigas
   - Reset de session state ao devolver conexão ao pool (`sp_reset_connection`)
   - Métricas: ativas, idle, pinned, criadas, destruídas, tempo de espera

3. **Lifecycle de Conexão:**
   ```
   Acquire → [wait in queue if full] → Use → Release
                                                 ↓
                                    sp_reset_connection → Return to pool
                                                 ↓
                                    [evict if too old or broken]
   ```

4. **sp_reset_connection**: Quando uma conexão é devolvida ao pool, o proxy executa `sp_reset_connection` no SQL Server para limpar o session state (temp tables, SET options, etc.), permitindo reutilização segura.

5. **Testes unitários** do pool manager

### 1.2 Entregáveis

- [ ] `internal/pool/pool.go` — Pool manager genérico
- [ ] `internal/pool/connection.go` — Wrapper de conexão com metadata (pinned, bucket_id, created_at, last_used)
- [ ] `internal/pool/health.go` — Health checker de conexões (SELECT 1 + sp_reset_connection)
- [ ] `internal/config/config.go` — Loader de configuração
- [ ] Testes unitários com cobertura > 80%

**Estimativa:** 2-3 dias

---

## Fase 2 — Proxy TDS (Wire Protocol SQL Server)

**Objetivo:** Implementar o proxy TCP que fala o protocolo TDS, sendo transparente para a coreVM.

### 2.1 Sobre o Protocolo TDS

O TDS (Tabular Data Stream) é o wire protocol do SQL Server. A especificação é pública ([MS-TDS](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-tds/)). O proxy precisa implementar apenas um **subset mínimo** do protocolo:

```
Fases da Conexão TDS:
═══════════════════════════════════════════════
1. Pre-Login    → Negocia versão TDS e encryption
2. Login7       → Autenticação + database name (usado para routing)
3. Data Phase   → Troca de pacotes SQL Batch, RPC, Response
4. Attention    → Cancelamento de queries
5. Logout       → Encerramento da conexão
```

### 2.2 Estratégia de Routing

O proxy usa uma **porta única (1433)** e faz routing pelo **database name** informado no pacote Login7:

```
Login7 Packet:
┌─────────────────────────────────────────────┐
│  ... | Database: "bucket_001_tenantdb" | ...│
└─────────────────────────────────────────────┘
                    ↓
         Proxy extrai database name
                    ↓
         Mapeia para bucket-001
                    ↓
         Roteia para sqlserver-bucket-1:1433
```

**Alternativa:** Se os database names não identificam o bucket de forma previsível, pode-se usar mapeamento explícito no `buckets.yaml` ou routing por server name alias.

### 2.3 Tarefas

1. **TCP Listener** — Escutar na porta 1433, aceitar conexões TDS

2. **TDS Pre-Login Handler:**
   - Receber pacote Pre-Login do cliente
   - Parsear campos de versão e encryption
   - Neste ponto, o proxy ainda não sabe o bucket — mantém conexão em estado "pending"

3. **TDS Login7 Handler:**
   - Receber pacote Login7 do cliente
   - Extrair **database name**, **username**, e **server name** do pacote
   - Usar database name para identificar o bucket de destino
   - Adquirir conexão do pool do bucket (ou esperar na fila)
   - Forward do Login7 para o SQL Server backend (ou usar conexão já autenticada do pool)

4. **TDS Packet Relay (Data Phase):**
   - Proxy bidirecional de pacotes TDS
   - Ler header TDS (8 bytes) → determinar tipo e tamanho → forward
   - **Não** parsear conteúdo SQL — apenas forward bytes brutos
   - Detectar tipos de pacote para connection pinning (ver seção 2.4)

5. **Attention Handler:**
   - Quando o cliente envia pacote Attention (cancel query), forward para backend
   - Aguardar resposta de Attention Ack antes de continuar

6. **Connection Lifecycle completo:**
   ```
   Cliente conecta (TCP)
        ↓
   Pre-Login (proxy ↔ cliente)
        ↓
   Login7 → Extrair database → Identificar bucket → Acquire pool conn
        ↓
   Forward Login7 para backend OU usar conn autenticada do pool
        ↓
   ┌─── Data Phase (loop) ──────────────────────────────────┐
   │ Cliente envia SQL Batch/RPC → Forward → Response back  │
   │ Detectar BEGIN TRAN / sp_prepare → Pin connection      │
   │ Detectar COMMIT/ROLLBACK / sp_unprepare → Unpin        │
   └────────────────────────────────────────────────────────┘
        ↓
   Cliente desconecta → sp_reset_connection → Return conn to pool
   ```

### 2.4 Connection Pinning Detection

O proxy faz inspeção mínima dos pacotes TDS para detectar pinning:

| Tipo de Pacote TDS | Byte ID | O que o proxy detecta |
|---|---|---|
| `SQL_BATCH` | `0x01` | Inspeciona superficialmente para `BEGIN TRAN`, `COMMIT`, `ROLLBACK` |
| `RPC` | `0x03` | Extrai proc name: `sp_prepare`, `sp_execute`, `sp_unprepare`, `sp_executesql` |
| `TRANSACTION_MANAGER` | `0x0E` | Ação: Begin, Commit, Rollback, Savepoint |
| `ATTENTION` | `0x06` | Cancel — forward e aguardar ack |
| `BULK_LOAD` | `0x07` | Pin connection durante bulk load |

### 2.5 Tratamento de TLS/Encryption

SQL Server suporta 4 modos de encryption no Pre-Login:

| Modo | Comportamento do Proxy |
|---|---|
| `ENCRYPT_OFF` | Sem TLS — proxy faz relay direto |
| `ENCRYPT_ON` | TLS full — proxy precisa terminar TLS do cliente e abrir TLS para backend (TLS termination) |
| `ENCRYPT_NOT_SUP` | Sem encryption — relay direto |
| `ENCRYPT_REQ` | TLS obrigatório — TLS termination |

**Para a POC:** Iniciar com `ENCRYPT_OFF` para simplificar. TLS termination pode ser adicionado na Fase 7 (hardening).

### 2.6 Entregáveis

- [ ] `internal/tds/packet.go` — Parser de pacotes TDS (header, tipos)
- [ ] `internal/tds/prelogin.go` — Handler do Pre-Login
- [ ] `internal/tds/login7.go` — Parser do Login7 (extrair database, username)
- [ ] `internal/tds/relay.go` — Relay bidirecional de pacotes TDS
- [ ] `internal/tds/pinning.go` — Detector de connection pinning (transaction, prepared stmts)
- [ ] `internal/proxy/listener.go` — TCP listener TDS
- [ ] `internal/proxy/handler.go` — Connection handler (lifecycle completo)
- [ ] `internal/proxy/router.go` — Routing por bucket (baseado em database name)
- [ ] Teste de integração: conectar via `sqlcmd` → proxy → container SQL Server
- [ ] Teste: executar DDL, stored procedure, prepared statement, transaction via proxy

**Estimativa:** 4-5 dias

> **Nota:** Esta é a fase mais complexa. O protocolo TDS é bem documentado pela Microsoft, mas a implementação do subset mínimo necessário para proxy (especialmente Login7 parsing e pinning detection) requer atenção. Referências úteis:
> - [MS-TDS Spec](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-tds/)
> - [go-mssqldb source](https://github.com/microsoft/go-mssqldb) — contém implementação TDS em Go
> - [FreeTDS](https://www.freetds.org/) — implementação open-source de referência do TDS

---

## Fase 3 — Coordenação Distribuída (Redis)

**Objetivo:** Permitir que múltiplas instâncias do proxy compartilhem estado de conexões via Redis.

### 3.1 Tarefas

1. **Redis Client** — Integração com Redis usando `github.com/redis/go-redis/v9`

2. **Atomic Counter Manager:**
   ```go
   // Lua script atômico para acquire — roda atomicamente no Redis
   local current = redis.call('GET', KEYS[1])
   local max = redis.call('GET', KEYS[2])
   if tonumber(current or 0) < tonumber(max) then
       return redis.call('INCR', KEYS[1])
   end
   return -1
   ```

3. **Distributed Queue** — Fila de espera via Redis:
   - Pub/Sub para notificação de liberação de conexão
   - Semáforo distribuído com timeout
   - Heartbeat das instâncias (para detectar instâncias mortas e corrigir contadores)

4. **Consistency Model:**
   - O contador no Redis reflete conexões **em uso por todas as instâncias**
   - Se uma instância morre sem decrementar, o heartbeat detecta e corrige (TTL nos contadores por instância)
   - Lua scripts garantem atomicidade no check-and-increment
   - Cada instância registra suas conexões ativas em um hash por instância para auditoria:
     ```
     instance:{instance_id}:connections → hash { bucket_id: count }
     ```

5. **Fallback Mode (Redis indisponível):**
   - Cada instância opera com limite local: `max_connections / número_de_instâncias`
   - Alerta é disparado via métricas
   - Quando Redis volta, reconcilia contadores

### 3.2 Entregáveis

- [ ] `internal/coordinator/redis.go` — Redis coordinator
- [ ] `internal/coordinator/semaphore.go` — Semáforo distribuído
- [ ] `internal/coordinator/heartbeat.go` — Heartbeat de instâncias
- [ ] `internal/coordinator/lua/acquire.lua` — Script Lua para acquire atômico
- [ ] `internal/coordinator/lua/release.lua` — Script Lua para release atômico
- [ ] `internal/queue/distributed.go` — Fila distribuída
- [ ] Teste de integração: 3 instâncias do proxy respeitando limite global de conexões por bucket

**Estimativa:** 3-4 dias

---

## Fase 4 — Fila de Espera e Backpressure

**Objetivo:** Implementar o mecanismo de enfileiramento quando o limite de conexões é atingido.

### 4.1 Tarefas

1. **Wait Queue por Bucket:**
   - FIFO com timeout configurável (default: 30s)
   - Métricas: tamanho da fila, tempo médio de espera, timeouts
   - Circuit breaker: se a fila exceder tamanho máximo, rejeitar com erro TDS ao cliente

2. **Notification System:**
   - Quando uma conexão é liberada, notificar o próximo na fila
   - Usar Go channel internamente + Redis Pub/Sub entre instâncias
   - Anti-thundering-herd: notificar apenas 1 waiter por conexão liberada

3. **Graceful Degradation:**
   - Timeout progressivo: tempo de espera aumenta conforme a fila cresce (exponential backoff)
   - Métricas de saturação para triggers de auto-scaling
   - Resposta TDS Error ao cliente quando timeout é atingido (error number + message legível)

### 4.2 Entregáveis

- [ ] `internal/queue/queue.go` — Queue manager
- [ ] `internal/queue/waiter.go` — Waiter com timeout, cancelamento e contexto Go
- [ ] `internal/tds/error.go` — Gerador de pacotes TDS Error (para enviar erro ao cliente quando timeout)
- [ ] Testes de carga mostrando comportamento da fila sob pressão

**Estimativa:** 2 dias

---

## Fase 5 — Observabilidade

**Objetivo:** Dashboards e métricas completas para validar a POC.

### 5.1 Métricas Prometheus

| Métrica | Tipo | Labels |
|---|---|---|
| `proxy_connections_active` | Gauge | `bucket_id` |
| `proxy_connections_idle` | Gauge | `bucket_id` |
| `proxy_connections_pinned` | Gauge | `bucket_id`, `pin_reason` (transaction/prepared) |
| `proxy_connections_total` | Counter | `bucket_id`, `status` (acquired/released/timeout/error) |
| `proxy_connections_max` | Gauge | `bucket_id` |
| `proxy_queue_length` | Gauge | `bucket_id` |
| `proxy_queue_wait_seconds` | Histogram | `bucket_id` |
| `proxy_tds_packets_total` | Counter | `bucket_id`, `direction` (client/server), `type` (sql_batch/rpc/etc) |
| `proxy_query_duration_seconds` | Histogram | `bucket_id` |
| `proxy_connection_errors_total` | Counter | `bucket_id`, `error_type` |
| `proxy_redis_operations_total` | Counter | `operation`, `status` |
| `proxy_instance_heartbeat` | Gauge | `instance_id` |
| `proxy_pinning_duration_seconds` | Histogram | `bucket_id`, `pin_reason` |

### 5.2 Dashboard Grafana

- **Painel 1 — Visão Geral:** Conexões ativas vs máximo por bucket (gauge bars)
- **Painel 2 — Pinning:** Conexões pinned por tipo (transaction vs prepared) por bucket
- **Painel 3 — Fila de Espera:** Tamanho e tempo médio por bucket
- **Painel 4 — Latência:** P50, P95, P99 do proxy
- **Painel 5 — Erros:** Timeouts, connection errors, TDS errors
- **Painel 6 — Redis:** Operações, latência, disponibilidade
- **Painel 7 — Instâncias:** Conexões por instância, heartbeat status

### 5.3 Entregáveis

- [ ] `internal/metrics/metrics.go` — Registro de métricas Prometheus
- [ ] `prometheus/prometheus.yml` — Config Prometheus
- [ ] `grafana/dashboards/proxy-dashboard.json` — Dashboard completo
- [ ] Alerting rules básicas (queue > threshold, connection errors > threshold)

**Estimativa:** 1-2 dias

---

## Fase 6 — Load Generator e Testes de Carga

**Objetivo:** Simular o comportamento de milhares de coreVMs para validar a POC.

### 6.1 Load Generator (Go)

Ferramenta customizada em Go que simula coreVMs conectando via TDS (usando `go-mssqldb` como driver):

```
Parâmetros:
  --total-connections    1000    # Total de conexões simultâneas a simular
  --buckets             5       # Número de buckets para distribuir
  --query-interval      100ms   # Intervalo entre queries por conexão
  --query-mix           mixed   # Tipo de queries: simple|prepared|transaction|mixed
  --connection-duration  5m     # Tempo que cada "app" fica conectado (baseado no padrão)
  --ramp-up             60s     # Tempo de ramp-up
  --include-ddl         false   # Incluir DDL no mix de queries
  --include-sprocs      true    # Incluir stored procedures no mix
```

**Query Mix (modo `mixed`):**
- 60% queries simples (SELECT, INSERT, UPDATE)
- 15% prepared statements (sp_prepare → sp_execute × N → sp_unprepare)
- 15% transações (BEGIN TRAN → N queries → COMMIT/ROLLBACK)
- 5% stored procedures (EXEC sp_name)
- 5% DDL (CREATE TABLE #temp, ALTER, etc.)

### 6.2 Cenários de Teste

| # | Cenário | Objetivo |
|---|---|---|
| 1 | **Baseline** | 200 conexões/bucket, limite de 50 → validar que fila funciona |
| 2 | **Burst** | 0 → 500 conexões em 5s → validar backpressure |
| 3 | **Steady State** | Carga constante por 30min → validar estabilidade e memory leaks |
| 4 | **Instance Failure** | Derrubar 1 instância do proxy durante carga → validar HA e correção de contadores |
| 5 | **Redis Failure** | Derrubar Redis durante carga → validar fallback mode |
| 6 | **Uneven Distribution** | 80% da carga em 1 dos 5 buckets → validar isolamento |
| 7 | **Scale Out** | Adicionar instância durante carga → validar coordenação |
| 8 | **Long Transactions** | Transações de 30s+ → validar pinning e impacto no pool |
| 9 | **Prepared Statement Storm** | 100% prepared statements → validar pinning sob pressão |

### 6.3 Entregáveis

- [ ] `cmd/loadgen/main.go` — Load generator com suporte a TDS/SQL Server
- [ ] `scripts/run-loadtest.sh` — Script de execução dos cenários
- [ ] Relatório com resultados de cada cenário (latência, fila, erros, uso de memória)

**Estimativa:** 2-3 dias

---

## Fase 7 — Hardening e Documentação

### 7.1 Hardening

- [ ] Suporte a TLS termination (Pre-Login encryption negotiation)
- [ ] Graceful shutdown (drain connections antes de parar)
- [ ] Connection leak detector (conexões pinned por mais de X minutos)
- [ ] Retry automático em falhas de conexão backend transientes
- [ ] Rate limiting por tenant no proxy (usando o rate limit existente de ~10 chamadas/min como referência)

### 7.2 Documentação

- [ ] README.md completo com instruções de execução
- [ ] Documentação de arquitetura (diagramas)
- [ ] Guia de configuração de buckets
- [ ] Runbook de operação (troubleshooting)
- [ ] Análise de performance (resultados dos testes)

**Estimativa:** 2-3 dias

---

## Cronograma Consolidado

| Fase | Descrição | Estimativa | Dependências |
|---|---|---|---|
| **0** | Setup e Infra Docker (SQL Server) | 1-2 dias | — |
| **1** | Connection Pool Manager | 2-3 dias | Fase 0 |
| **2** | Proxy TDS (Wire Protocol SQL Server) | 4-5 dias | Fase 1 |
| **3** | Coordenação Distribuída (Redis) | 3-4 dias | Fase 1 |
| **4** | Fila de Espera e Backpressure | 2 dias | Fases 2, 3 |
| **5** | Observabilidade | 1-2 dias | Fase 2 |
| **6** | Load Generator e Testes | 2-3 dias | Fase 4, 5 |
| **7** | Hardening e Documentação | 2-3 dias | Fase 6 |
| | **Total** | **17-25 dias úteis** | |

> **Nota:** A estimativa aumentou em relação à versão anterior (era 15-22) porque o protocolo TDS é mais complexo que MySQL/PG para implementar como proxy, especialmente a detecção de pinning e o tratamento de transações via Transaction Manager Request.

---

## Decisões Técnicas e Recomendações

### Redis como Coordenador: É suficiente?

**Sim, Redis é suficiente para esta finalidade**, e é a escolha correta. Justificativas:

| Aspecto | Análise |
|---|---|
| **Latência** | Redis opera em sub-milissegundo (~0.1-0.5ms) para operações atômicas. Desprezível comparado à latência de uma query SQL Server. |
| **Atomicidade** | Lua scripts no Redis são executados atomicamente (single-threaded). `INCR`/`DECR` e check-and-set são naturalmente atômicos. |
| **Throughput** | Redis single-node suporta 100k+ operações/s. Para 120 buckets com acquire/release, a carga no Redis será muito baixa. |
| **Alta Disponibilidade** | Redis Sentinel ou Redis Cluster garantem failover automático. |
| **Alternativas descartadas** | etcd (overhead de consenso desnecessário), ZooKeeper (complexidade operacional), DynamoDB (latência de rede). |

### Sobre a complexidade do TDS vs MySQL/PG

O protocolo TDS traz desafios adicionais comparado ao MySQL/PG:

| Aspecto | MySQL/PG | TDS (SQL Server) |
|---|---|---|
| **Documentação** | Comunidade extensa | Especificação formal da Microsoft ([MS-TDS]) — completa mas densa |
| **Implementações de referência em Go** | go-mysql, pgproto3 (abundantes) | go-mssqldb (mais como driver, não como proxy) |
| **Handshake** | Relativamente simples | Pre-Login + Login7 + TLS negotiation — mais complexo |
| **Transactions** | Via SQL text | Via SQL text **E** via Transaction Manager Request (tipo de pacote separado) |
| **Prepared Statements** | Via protocol commands | Via RPC (`sp_prepare`/`sp_execute`) — requer parsing do nome da proc |
| **Session Reset** | `RESET CONNECTION` / `DISCARD ALL` | `sp_reset_connection` — funciona bem |

**Mitigação:** O código-fonte do `go-mssqldb` contém uma implementação funcional do TDS em Go que pode ser usada como referência direta para o parsing de pacotes.

---

## Análise de Viabilidade (Atualizada para SQL Server)

### O proxy consegue lidar com 300k aplicativos conectando em SQL Server?

**Resposta: Sim, é viável, com os mesmos fundamentos da análise anterior, mais considerações específicas do SQL Server.**

### Considerações Específicas do SQL Server

#### 1. Worker Threads

SQL Server usa um pool de worker threads (default ~512) para processar conexões ativas. Conexões que **não estão executando queries** não consomem worker threads. Isso é **a favor** do proxy:
- O proxy pode manter muitas conexões lógicas (coreVMs) e poucas conexões físicas (pool)
- As conexões físicas no pool ficam idle quando não estão executando queries
- Reduz drasticamente o consumo de worker threads no SQL Server

#### 2. Memory Grants

SQL Server aloca memory grants para queries em execução (sort, hash join, etc.). O proxy **não muda** esse comportamento, mas ao **limitar conexões simultâneas**, automaticamente limita a quantidade de memory grants simultâneos — que é exatamente o objetivo.

#### 3. Connection Pinning Impact

O uso de prepared statements e transactions pela coreVM **reduz a eficiência do pooling**, porque conexões pinned não podem ser reutilizadas. Cenários de impacto:

| Cenário | Impacto no Pooling |
|---|---|
| Queries simples (60% do mix) | ✅ Excelente — acquire e release imediato |
| Prepared statements curtos (<1s) | ⚠️ Moderado — pin durante o prepare/execute/unprepare |
| Transações curtas (<5s) | ⚠️ Moderado — pin durante BEGIN→COMMIT |
| Transações longas (>30s) | ❌ Alto — conexão bloqueada por longo tempo |
| Session state changes (SET options) | ❌ Problemático — pode exigir pin por toda a sessão |

**Recomendação:** Monitorar o ratio de conexões pinned vs livres nos testes. Se >70% das conexões ficarem pinned o tempo todo, considerar modo "session pooling" ao invés de "transaction pooling".

#### 4. sp_reset_connection

SQL Server oferece `sp_reset_connection` (chamado internamente pelo driver `go-mssqldb` ao reutilizar conexões) que limpa:
- Temp tables da sessão
- SET options alterados
- Locks mantidos
- Cursors abertos

Isso é **essencial** para reutilizar conexões com segurança no pool.

#### 5. Comparação com Soluções Existentes para SQL Server

| Solução | Serve? | Por que sim/não |
|---|---|---|
| **AWS RDS Proxy** | ❌ Não | RDS Proxy **não suporta SQL Server** — apenas MySQL e PostgreSQL |
| **ProxySQL** | ❌ Não | Apenas MySQL |
| **PgBouncer** | ❌ Não | Apenas PostgreSQL |
| **.NET SqlClient Connection Pooling** | ❌ Não | Pool local por processo — sem coordenação distribuída |
| **Solução Custom (Go)** | ✅ **Sim** | Única alternativa que atende todos os requisitos |

> **Importante:** Ao contrário de MySQL/PostgreSQL, **não existe um proxy de connection pooling maduro e open-source para SQL Server**. Isso torna a solução custom não apenas justificada, mas **necessária**. Não há alternativa off-the-shelf.

### Veredito Final de Viabilidade

> **A solução é tecnicamente viável e necessária.** Diferentemente de MySQL/PG, onde existem ferramentas maduras (ProxySQL, PgBouncer), o ecossistema SQL Server **não possui um proxy de connection pooling distribuído**. O AWS RDS Proxy nem sequer suporta SQL Server.
>
> Go é a linguagem ideal para implementar este proxy: excelente para I/O-bound workloads, goroutines para alta concorrência, e existe uma implementação de referência do protocolo TDS em Go (`go-mssqldb`).
>
> **Desafios principais:**
> 1. **Complexidade do TDS** — mais complexo que MySQL/PG, mas bem documentado pela Microsoft
> 2. **Connection Pinning** — prepared statements e transactions reduzem a eficiência do pooling; monitorar ratio pinned/free
> 3. **Corretude** — garantir que contadores nunca dessincronizem e que conexões nunca vazem
>
> **Estimativa de infra para produção:** Para 300k apps em 120 buckets, **4-8 instâncias do proxy** (cada uma com 4-8 vCPUs, 8-16GB RAM) seriam suficientes, com capacidade de escalar para 12-16 instâncias em picos.

---

## Próximos Passos

1. ✅ ~~Validar premissas~~ — **Validadas** (SQL Server, zero mudança na coreVM, mixed prepared/unprepared, DDL+stored procs)
2. **Revisar este plano atualizado** — ajustar estimativas, scope ou prioridades se necessário
3. **Iniciar Fase 0** — setup do projeto Go e Docker Compose com SQL Server
