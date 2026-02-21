# 🌐 Guia de Tradução de Comentários — Inglês → Português Brasileiro

> **Objetivo:** Traduzir todos os comentários de código de inglês para português brasileiro,
> mantendo 100% do código inalterado.

---

## 📌 Regras Gerais

| # | Regra | Exemplo |
|---|-------|---------|
| 1 | **Somente comentários** são traduzidos — strings de log, flags, variáveis e imports **nunca** são alterados | `log.Println("Starting...")` → NÃO traduzir |
| 2 | Manter a **formatação de banners** `─── ... ───` idêntica | `// ─── Carregar Configuração ───...` |
| 3 | Preservar o prefixo `Phase N —` como `Fase N —` | `Phase 1 —` → `Fase 1 —` |
| 4 | Doc de pacote segue a convenção Go: iniciar com `// Package <nome>` | `// Package main é o ponto de entrada...` |
| 5 | Doc de função/tipo segue a convenção Go: iniciar com `// NomeDaFunção ...` | `// NewChecker cria um novo verificador de saúde.` |
| 6 | Comentários inline (`// texto` no final da linha de código) devem ser traduzidos mantendo o alinhamento | `x := 1 // contador` |
| 7 | Referências a seções do protocolo MS-TDS (ex: `MS-TDS 2.2.6.4`) **não** são traduzidas | Manter como está |
| 8 | Acrônimos conhecidos mantêm a forma original | TDS, TLS, UTF-16, LE, LIFO, TTL, SHA |

---

## 🔒 Termos Técnicos que NÃO Devem Ser Traduzidos

Estes termos são consagrados no domínio e devem permanecer em inglês:

| Termo | Motivo |
|-------|--------|
| `pool` / `connection pool` | Termo padrão de infraestrutura |
| `bucket` | Nome do conceito no domínio deste projeto |
| `proxy` | Termo universal |
| `heartbeat` | Termo padrão de sistemas distribuídos |
| `health check` | Termo padrão de observabilidade |
| `metrics` | Termo padrão de observabilidade |
| `endpoint` | Termo universal de APIs |
| `shutdown` | Termo padrão de ciclo de vida |
| `fallback` | Termo padrão de resiliência |
| `callback` | Termo padrão de programação |
| `token` | Termo do protocolo TDS |
| `relay` | Termo do domínio proxy |
| `pinning` / `pin` / `unpin` | Conceito específico de connection pinning |
| `goroutine` | Conceito específico de Go |
| `channel` | Conceito específico de Go |
| `context` | Conceito específico de Go |
| `idle` | Termo padrão de pool de conexões |
| `stale` | Termo padrão de eviction |
| `waiter` | Conceito do pool (quem espera conexão) |
| `scrape` | Termo do Prometheus |
| `Pub/Sub` | Termo padrão Redis |
| `circuit breaker` | Padrão de resiliência |
| `semaphore` | Primitiva de concorrência |
| `hash` | Termo universal |
| `payload` | Termo de protocolo |
| `offset` | Termo de protocolo |
| `header` | Termo de protocolo |
| `TDS`, `TLS`, `SSPI` | Protocolos/padrões |
| `Login7`, `Pre-Login` | Tipos de pacote TDS |
| `sp_reset_connection` | Stored procedure do SQL Server |
| `ENVCHANGE`, `DONE`, `DONE_INXACT` | Tokens TDS |
| `ALL_HEADERS` | Estrutura TDS |
| `Lua` | Linguagem de scripting |
| `draft` | Pull request draft |
| `label` | Termo do Prometheus |

---

## ✅ Termos que DEVEM Ser Traduzidos

| Inglês | Português |
|--------|-----------|
| `Initialize` / `Initialization` | `Inicializar` / `Inicialização` |
| `Load` / `Loading` | `Carregar` / `Carregamento` |
| `Configuration` | `Configuração` |
| `Connection` | `Conexão` |
| `Manager` | `Gerenciador` |
| `Checker` | `Verificador` |
| `Distributed` | `Distribuído(a)` |
| `Queue` | `Fila` |
| `Acquire` / `Release` | `Adquirir` / `Liberar` |
| `Create` / `Close` | `Criar` / `Fechar` |
| `Returns` | `Retorna` |
| `Listener` | `Listener` (manter — termo Go) |
| `Handler` | `Handler` (manter — termo Go) |
| `Router` / `Route` / `Routing` | `Roteador` / `Rota` / `Roteamento` |
| `Graceful` | `Gracioso(a)` |
| `Lifecycle` | `Ciclo de vida` |
| `Background` | `Segundo plano` (ou manter `background` se inline) |
| `Server` | `Servidor` |
| `Client` | `Cliente` |
| `Instance` | `Instância` |
| `Slot` | `Slot` (manter — termo do domínio) |
| `Error` / `Failure` | `Erro` / `Falha` |
| `Timeout` | `Timeout` (manter — universalmente usado) |
| `Cleanup` | `Limpeza` |
| `Dead` | `Morto(a)` / `Inativo(a)` |
| `Orphaned` | `Órfão(s)` |
| `Eviction` / `Evict` | `Evição` / `Remover por obsolescência` |
| `Maintenance` | `Manutenção` |
| `Statistics` / `Stats` | `Estatísticas` |
| `Reverse order` | `Ordem reversa` |
| `Severity` | `Severidade` |

---

## 📝 Exemplos Antes/Depois

### Doc de pacote

**Antes:**
```go
// Package main is the entrypoint for the connection pooling proxy.
// It loads configuration, initializes health checks and metrics,
// and sets up graceful shutdown handling.
package main
```

**Depois:**
```go
// Package main é o ponto de entrada do proxy de connection pooling.
// Carrega a configuração, inicializa health checks e métricas,
// e configura o tratamento de shutdown gracioso.
package main
```

### Banner de seção

**Antes:**
```go
// ─── Load Configuration ───────────────────────────────────────────
```

**Depois:**
```go
// ─── Carregar Configuração ────────────────────────────────────────
```

### Comentário explicativo

**Antes:**
```go
// Pre-register metric labels for each bucket so Grafana shows them immediately
```

**Depois:**
```go
// Pré-registrar labels de métricas para cada bucket para que o Grafana os exiba imediatamente
```

### Comentário inline

**Antes:**
```go
continue // skip ourselves
```

**Depois:**
```go
continue // pular nós mesmos
```

### Doc de função com termos técnicos preservados

**Antes:**
```go
// Acquire obtains a connection from the pool. If no connection is available
// and the pool is at max capacity, the caller blocks until a connection is
// released or the context expires.
```

**Depois:**
```go
// Acquire obtém uma conexão do pool. Se nenhuma conexão estiver disponível
// e o pool estiver na capacidade máxima, o chamador bloqueia até que uma
// conexão seja liberada ou o context expire.
```

### Referência a protocolo (não traduzir referência)

**Antes:**
```go
// ── TDS Packet Types (MS-TDS 2.2.3.1) ─────────────────────────
```

**Depois:**
```go
// ── Tipos de Pacote TDS (MS-TDS 2.2.3.1) ──────────────────────
```

---

## ⚠️ O que NUNCA Alterar

1. **Strings de log** — `log.Println("...")`, `log.Printf("...")`, `log.Fatalf("...")`
2. **Strings de flag** — `flag.String("config", "...", "Path to ...")`
3. **Nomes de variáveis, funções, tipos, pacotes** — são código
4. **Imports** — são código
5. **Constantes string** — `const foo = "..."`
6. **Tags de struct** — `` `yaml:"..." json:"..."` ``
7. **Mensagens de erro** — `fmt.Errorf("...")`, `errors.New("...")`
8. **Nomes de arquivos em comentários** — ex: `// See proxy.yaml`

---

## 🔄 Fluxo de Trabalho

1. Solicitar: *"Traduza os comentários de `internal/pool/pool.go`"*
2. O agente aplica as regras deste guia
3. Executar `go build ./cmd/... ./internal/... ./pkg/...` para validar
4. Revisar e aprovar
5. Próximo arquivo

---

## 📊 Progresso

| Arquivo | Comentários | Status |
|---------|-------------|--------|
| `cmd/proxy/main.go` | 14 | ✅ Concluído |
| `cmd/loadgen/main.go` | 2 | ✅ Concluído |
| `internal/coordinator/redis.go` | 50 | ✅ Concluído |
| `internal/coordinator/heartbeat.go` | 20 | ✅ Concluído |
| `internal/coordinator/semaphore.go` | 15 | ✅ Concluído |
| `internal/tds/pinning.go` | 44 | ✅ Concluído |
| `internal/tds/packet.go` | 33 | ✅ Concluído |
| `internal/tds/error.go` | 30 | ✅ Concluído |
| `internal/tds/prelogin.go` | 25 | ✅ Concluído |
| `internal/tds/login7.go` | 18 | ✅ Concluído |
| `internal/tds/relay.go` | 15 | ✅ Concluído |
| `internal/pool/pool.go` | 42 | ✅ Concluído |
| `internal/pool/connection.go` | 30 | ✅ Concluído |
| `internal/pool/manager.go` | 10 | ✅ Concluído |
| `internal/pool/health.go` | 2 | ✅ Concluído |
| `internal/proxy/handler.go` | 28 | ✅ Concluído |
| `internal/proxy/router.go` | 20 | ✅ Concluído |
| `internal/proxy/listener.go` | 19 | ✅ Concluído |
| `internal/queue/distributed.go` | 19 | ✅ Concluído |
| `internal/health/health.go` | 15 | ✅ Concluído |
| `internal/metrics/metrics.go` | 14 | ✅ Concluído |
| `internal/config/config.go` | 12 | ✅ Concluído |
| `pkg/bucket/bucket.go` | 5 | ✅ Concluído |
| **TOTAL** | **468** | **468/468 (100%) ✅** |
