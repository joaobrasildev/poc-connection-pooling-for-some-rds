# DECISIONS.md — Registro de Decisões Técnicas

> **Propósito:** evitar que o agente de IA sugira refatorar algo que já foi decidido
> por um bom motivo, e preservar o "porquê" por trás de escolhas não-óbvias.
>
> **Formato:** ADR leve (Architecture Decision Record). Cada decisão tem contexto,
> alternativas consideradas, escolha feita e consequências.
>
> **Última atualização:** 2026-02-18 (Fase 3 concluída)

---

## ADR-001: TCP Relay Transparente (io.Copy) em vez de TDS Parsing pós Pre-Login

**Fase:** 2 — TDS Wire Protocol Proxy \
**Status:** Aceita (em vigor) \
**Data:** 2026-02 (sessão de implementação da Fase 2)

### Contexto
O proxy precisa intermediar conexões TDS entre client e SQL Server. A abordagem
inicial era fazer parsing completo de todos os pacotes TDS (Pre-Login → Login7 →
Data Phase) com o proxy terminando TLS e re-encriptando.

### Problema
O SQL Server 2022 exige TLS por padrão (`ENCRYPT_ON`). Quando o proxy tentava
interceptar o Pre-Login e responder `ENCRYPT_NOT_SUP`, o driver do client e o
backend discordavam sobre o nível de encriptação, causando falha de TLS handshake.
Tentar fazer TLS termination no proxy introduzia complexidade enorme (certificados,
re-encrypt, parsing de TDS dentro de TLS records).

### Decisão
Após o Pre-Login, o proxy faz **relay TCP transparente** via `io.Copy` bidirecional.
O Pre-Login é o único pacote TDS que o proxy lê e faz forward. Tudo depois
(TLS handshake, Login7, queries, respostas) passa como bytes opacos.

### Alternativas descartadas
1. **TLS termination no proxy** — complexidade desproporcional para um POC
2. **Forçar ENCRYPT_NOT_SUP** — SQL Server 2022 rejeita, clients modernos também
3. **Parsing TDS dentro de TLS records** — impossível sem terminar TLS primeiro

### Consequências
- ✅ Funciona com qualquer configuração de TLS do SQL Server
- ✅ Zero overhead de parsing durante transferência de dados
- ❌ O proxy **não consegue inspecionar pacotes** durante a fase de dados (TLS opaco)
- ❌ **Pinning detection não funciona** com `io.Copy` — precisa de solução diferente na Fase 4
- ❌ **Routing por Login7 não funciona** — o Login7 está dentro do stream TLS
- ❌ Métricas TDS (`TDSPacketsTotal`, `QueryDuration`) não são populadas

### Impacto nas próximas fases
Para a Fase 4 (pinning), as opções são:
1. Implementar TLS termination no proxy (complexo, mas habilita tudo)
2. Usar `tds.Relay` com callback apenas em modo `ENCRYPT_NOT_SUP` (limitado)
3. Implementar pinning inferido (pin por sessão inteira, sem detecção granular)
4. Usar informações out-of-band (query no SQL Server para detectar transações abertas)

---

## ADR-002: Lua Scripts via EVALSHA em vez de Redis Transactions

**Fase:** 3 — Coordenação Distribuída \
**Status:** Aceita (em vigor)

### Contexto
O acquire de conexão precisa ser atômico: verificar `count < max` e incrementar
em uma única operação. Duas abordagens principais no Redis:
MULTI/EXEC (transactions) ou Lua scripts (EVAL/EVALSHA).

### Decisão
Usar **Lua scripts embeddados** (`//go:embed`) executados via `EVALSHA`.

### Razões
1. **Atomicidade real** — Lua executa atomicamente no Redis, sem window entre GET e INCR
2. **Menos round-trips** — uma chamada faz GET+compare+INCR+HINCRBY, vs 4 comandos separados
3. **Scripts cachados** — `ScriptLoad` no startup, depois `EvalSha` (apenas o hash trafega)
4. **Lógica condicional** — o script retorna -1 (lotado) ou -2 (não configurado) sem extra RTT
5. **Efeitos colaterais** — release.lua faz PUBLISH no mesmo script (notificação + decrement atômicos)

### Alternativas descartadas
- **MULTI/EXEC** — não suporta lógica condicional (GET dentro de MULTI sempre retorna QUEUED)
- **WATCH/MULTI** — funciona mas com retry loop, mais complexo e mais lento sob contention
- **Redlock** — overkill para contagem atômica, apropriado para locks exclusivos

### Consequências
- ✅ Operação verdadeiramente atômica, sem race conditions
- ✅ Performance: 1 RTT por acquire/release
- ❌ Lua scripts não funcionam em Redis Cluster com keys em slots diferentes
  (aceitável: POC usa Redis standalone)

---

## ADR-003: Pool *sql.DB NÃO é Usado para Tráfego TDS

**Fase:** 1 + 2 \
**Status:** Aceita (em vigor)

### Contexto
A Fase 1 criou um pool de `*sql.DB` connections (via go-mssqldb) para cada bucket.
A Fase 2 implementou o proxy TDS usando `net.Conn` diretas (TCP relay).

### Decisão
O pool `*sql.DB` **continua existindo** mas NÃO é usado para o tráfego de dados TDS.
As sessões TDS usam `net.DialTimeout` para criar conexões TCP diretas ao backend.

### Razão
`*sql.DB` é uma abstração de alto nível que gerencia seu próprio pooling interno,
encapsula a conexão TDS, e não expõe o `net.Conn` subjacente. O proxy precisa
de acesso raw ao stream TCP para relay transparente. São dois níveis de abstração
incompatíveis.

### Para que o pool *sql.DB serve
1. **Health checks** — `PingContext(ctx)` / `SELECT 1` nas idle connections
2. **sp_reset_connection** — limpeza de estado ao devolver conexão ao pool
3. **Warm connections** — manter `min_idle` conexões pré-abertas

### Consequências
- ❌ O `MaxConnections` do BucketPool controla conexões `*sql.DB`, NÃO as sessões TDS ativas
- ✅ O `coordinator.Acquire/Release` no handler.go é quem controla o limite de sessões TDS
- ⚠️ Esses são dois pools separados — pode confundir na leitura do código

---

## ADR-004: Bucket Selecionado no Pre-Login (antes do Login7)

**Fase:** 2 \
**Status:** Aceita (limitação conhecida)

### Contexto
O protocolo TDS tem esta sequência: Pre-Login → TLS → Login7. O Router
(ADR-001) não consegue ler o Login7 porque ele está dentro do stream TLS.

### Decisão
O bucket é selecionado em `pickBucket()` antes do Login7, usando o **primeiro
bucket** da configuração como default. O Router implementado (`router.go`) está
pronto mas **não é ativado**.

### Consequências
- ❌ Todas as conexões vão para o bucket-001 (HAProxy distribui entre proxies,
  mas cada proxy sempre conecta ao mesmo backend)
- ✅ Para o POC com 3 buckets idênticos (mesmo schema), é aceitável
- 🔮 Para produção: seria necessário routing por IP do client, header customizado,
  ou SNI (Server Name Indication) no TLS ClientHello

---

## ADR-005: Fallback Mode com Divisor Local

**Fase:** 3 \
**Status:** Aceita (em vigor)

### Contexto
Quando o Redis fica indisponível, o proxy não pode coordenar limites globais.
Sem fallback, o proxy simplesmente recusaria todas as conexões.

### Decisão
**Fallback mode**: cada instância opera independentemente com um limite local
calculado como `max_connections / local_limit_divisor`. Com 3 proxies e divisor=3,
cada um permite até 16 conexões (50/3). No pior caso (todos os proxies em fallback),
o total máximo é 48 (abaixo do limite de 50).

### Razão do divisor=3
- Número de instâncias do proxy no POC = 3
- `50 / 3 ≈ 16` → total máximo em fallback = 48 < 50 ✅
- Se uma instância morrer, as outras 2 × 16 = 32 < 50 ✅
- Configurável via `fallback.local_limit_divisor` no proxy.yaml

### Reconciliação
Quando o Redis volta:
1. Heartbeat detecta Redis disponível → chama `ExitFallback()`
2. `ExitFallback()` faz re-ping + re-load scripts + `reconcileCounts()`
3. `reconcileCounts()` sincroniza contadores locais para o Redis via pipeline HSET

### Consequências
- ✅ Proxy continua servindo mesmo sem Redis
- ❌ Sem coordenação cross-instance durante fallback (pode exceder limite por bucket
  se novas instâncias subirem sem saber do divisor correto)
- ❌ `local_limit_divisor` é estático — deve ser ≥ número máximo de instâncias

---

## ADR-006: Heartbeat com Cleanup de Instâncias Mortas

**Fase:** 3 \
**Status:** Aceita (em vigor)

### Contexto
Se um proxy morre sem graceful shutdown (OOMKill, crash, rede), seus contadores
ficam "presos" no Redis. Conexões que ele tinha ativas nunca são liberadas,
reduzindo permanentemente a capacidade disponível.

### Decisão
Cada proxy faz heartbeat (`SET key TTL=30s`) a cada 10s. A cada 30s, cada proxy
vivo verifica se os outros proxies ainda têm heartbeat. Se não:
1. Lê os contadores do morto (`HGETALL proxy:instance:{id}:conns`)
2. Subtrai dos contadores globais (`DECRBY proxy:bucket:{id}:count`)
3. Remove o morto (`DEL` keys + `SREM` do set)
4. Corrige contadores negativos (proteção contra double-cleanup)

### Alternativas descartadas
- **Redis keyspace notifications** — confiável mas requer configuração extra no Redis
- **Lease-based (SETNX com TTL por conexão)** — muitas keys, overhead alto
- **Cleanup centralizado (um proxy eleito)** — precisa de leader election

### Consequências
- ✅ Recuperação automática em ~30s após morte de uma instância
- ✅ Qualquer proxy vivo pode fazer o cleanup (sem single point of failure)
- ❌ Possibilidade de double-cleanup se dois proxies detectam ao mesmo tempo
  (mitigado: `DECRBY` é idempotente se combinado com correção de negativos)
- ❌ Window de ~30s onde capacidade fica reduzida antes do cleanup

---

## ADR-007: Reutilizar DistributedQueue da Fase 3 em vez de Criar Novos Arquivos

**Fase:** 4 — Fila de Espera e Backpressure \
**Status:** Aceita \
**Data:** 2026-02-18

### Contexto
O plano original da Fase 4 listava dois novos arquivos (`queue.go`, `waiter.go`).
Porém, a Fase 3 já entregou `DistributedQueue` (integra Semaphore + Coordinator)
e `Semaphore.Wait()` (Pub/Sub + polling com timeout). Esses componentes já
implementavam ~70% do escopo da Fase 4.

### Decisão
**Evoluir a `DistributedQueue` existente** em vez de criar novos arquivos:
1. Adicionar `maxQueueSize` + circuit breaker (rejeição imediata quando fila cheia)
2. Adicionar `QueueError` tipado com `IsQueueFull()` / `IsQueueTimeout()`
3. Adicionar `ErrQueueTimeout` (50004) e `ErrQueueFull` (50005) em `tds/error.go`
4. Integrar `DistributedQueue` no `handler.go` (substituindo `coordinator.Acquire` direto)
5. Passar `DistributedQueue` via `main.go → Server → Session`

### Alternativas descartadas
- **Criar `queue.go` e `waiter.go` novos** — duplicaria lógica que já existe
  no Semaphore e DistributedQueue
- **Usar BucketPool.waitQueue para TDS** — BucketPool gerencia `*sql.DB`,
  não sessões TDS (ver ADR-003)

### Consequências
- ✅ Zero duplicação de código — evoluiu o que já existia
- ✅ Métricas `QueueLength` e `QueueWaitDuration` já estavam instrumentadas no Semaphore
- ✅ Circuit breaker previne acúmulo ilimitado de goroutines esperando
- ✅ Erros tipados permitem enviar TDS Error específico ao client (50004 vs 50005)
- ❌ `maxQueueSize` é por instância, não global — em 3 proxies, o total máximo
  na fila pode ser 3 × `max_queue_size`

---

## Template para Próximas Decisões

```markdown
## ADR-NNN: [Título da decisão]

**Fase:** N — [Nome da fase] \
**Status:** Proposta | Aceita | Substituída por ADR-XXX \
**Data:** YYYY-MM-DD

### Contexto
[O que motivou esta decisão? Qual problema precisava ser resolvido?]

### Decisão
[O que foi decidido. Seja específico.]

### Alternativas descartadas
[Lista com breve razão de cada descarte]

### Consequências
- ✅ [benefício]
- ❌ [tradeoff/limitação]
- ⚠️ [risco ou ponto de atenção]
```
