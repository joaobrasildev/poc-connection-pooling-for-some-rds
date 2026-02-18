# Human Context — POC Connection Pooling Proxy

> **Público-alvo:** desenvolvedor experiente que não conhece Go e nunca construiu uma solução de connection pooling.
> **Objetivo:** entender como o projeto funciona de ponta a ponta para poder aplicá-lo no contexto real com 120 buckets.

---

## Índice

1. [Visão geral do problema](#1-visão-geral-do-problema)
2. [Visão geral da solução](#2-visão-geral-da-solução)
3. [Estrutura de diretórios](#3-estrutura-de-diretórios)
4. [Go para quem não conhece Go](#4-go-para-quem-não-conhece-go)
5. [O Entrypoint — cmd/proxy/main.go](#5-o-entrypoint--cmdproxymainGo)
6. [Configuração — proxy.yaml e buckets.yaml](#6-configuração--proxyyaml-e-bucketsyaml)
7. [O que é um Bucket](#7-o-que-é-um-bucket)
8. [Fluxo de uma conexão passo a passo](#8-fluxo-de-uma-conexão-passo-a-passo)
9. [O protocolo TDS — o fio que conecta tudo](#9-o-protocolo-tds--o-fio-que-conecta-tudo)
10. [Pool local de conexões](#10-pool-local-de-conexões)
11. [Coordenação distribuída via Redis](#11-coordenação-distribuída-via-redis)
12. [Heartbeat e limpeza de instâncias mortas](#12-heartbeat-e-limpeza-de-instâncias-mortas)
13. [Fila de espera e Circuit Breaker](#13-fila-de-espera-e-circuit-breaker)
14. [Modo Fallback — quando o Redis cai](#14-modo-fallback--quando-o-redis-cai)
15. [Health Check e Métricas](#15-health-check-e-métricas)
16. [Infraestrutura Docker](#16-infraestrutura-docker)
17. [Erros TDS enviados ao cliente](#17-erros-tds-enviados-ao-cliente)
18. [Shutdown gracioso](#18-shutdown-gracioso)
19. [Mapa de arquivos — referência rápida](#19-mapa-de-arquivos--referência-rápida)
20. [Aplicando ao contexto real com 120 buckets](#20-aplicando-ao-contexto-real-com-120-buckets)

---

## 1. Visão geral do problema

Você tem **N bancos de dados SQL Server** (chamados de "buckets") e precisa limitar o número máximo de conexões simultâneas para cada um deles. O SQL Server tem um limite físico de conexões, e se várias aplicações abrirem conexões sem controle, o banco estoura.

O desafio é que existem **múltiplas instâncias do proxy** rodando ao mesmo tempo (horizontal scaling). Cada uma precisa saber quantas conexões as outras estão usando para que o **total global** nunca ultrapasse o limite configurado.

**Resumo:** este projeto é um **proxy TCP transparente** que fica entre a aplicação e o SQL Server, controlando quantas conexões simultâneas cada banco aceita — de forma coordenada entre múltiplas instâncias do proxy.

---

## 2. Visão geral da solução

```
┌──────────┐     ┌───────────┐     ┌──────────────┐     ┌────────────┐
│ App/     │────▶│ HAProxy   │────▶│ Proxy (Go)   │────▶│ SQL Server │
│ CoreVM   │     │ (L4 TCP)  │     │ (este projeto)│     │ (Bucket)   │
└──────────┘     └───────────┘     └──────┬───────┘     └────────────┘
                                          │
                                          ▼
                                   ┌─────────────┐
                                   │   Redis     │
                                   │ (contadores │
                                   │  globais)   │
                                   └─────────────┘
```

**Componentes:**

| Componente | Papel |
|---|---|
| **HAProxy** | Load balancer L4 (TCP). Distribui conexões TDS entre as instâncias do proxy usando `leastconn`. Na AWS, seria um NLB. |
| **Proxy (Go)** | O coração do projeto. Aceita conexões TDS, decide qual bucket usar, controla o limite de conexões, e faz relay transparente de dados. |
| **Redis** | Armazena contadores globais de conexões. Garante que 3 instâncias do proxy nunca abram mais do que `max_connections` juntas. |
| **SQL Server** | O banco de dados final. Cada instância é um "bucket". |

---

## 3. Estrutura de diretórios

```
.
├── cmd/proxy/main.go              ← ENTRYPOINT (função main)
├── configs/
│   ├── proxy.yaml                 ← Config do proxy (portas, timeouts, Redis)
│   └── buckets.yaml               ← Config dos buckets (hosts, senhas, limites)
├── internal/                      ← Código privado (não importável por outros projetos Go)
│   ├── config/config.go           ← Leitura e validação de YAML
│   ├── proxy/
│   │   ├── handler.go             ← Lógica de uma sessão TDS (o mais importante)
│   │   └── listener.go            ← Servidor TCP que aceita conexões
│   ├── pool/
│   │   ├── manager.go             ← Gerencia pools de todos os buckets
│   │   ├── pool.go                ← Pool de conexões de um bucket
│   │   ├── connection.go          ← Struct de uma conexão com metadados
│   │   └── health.go              ← Health check das conexões (SELECT 1)
│   ├── coordinator/
│   │   ├── redis.go               ← Coordenação distribuída (Acquire/Release via Lua)
│   │   ├── semaphore.go           ← Semáforo com Pub/Sub + polling
│   │   ├── heartbeat.go           ← Heartbeat + limpeza de instâncias mortas
│   │   └── lua/
│   │       ├── acquire.lua        ← Script atômico: "posso abrir conexão?"
│   │       └── release.lua        ← Script atômico: "liberei uma conexão"
│   ├── queue/
│   │   └── distributed.go         ← Fila de espera distribuída com circuit breaker
│   ├── tds/
│   │   └── error.go               ← Constrói pacotes TDS de erro para o cliente
│   ├── metrics/
│   │   └── metrics.go             ← Definição de todas as métricas Prometheus
│   └── health/
│       └── health.go              ← Endpoints HTTP /health, /health/live, /health/ready
├── pkg/bucket/
│   └── bucket.go                  ← Struct Bucket (público, reutilizável)
├── deployments/
│   ├── docker-compose.yml         ← Orquestração de todos os containers
│   └── haproxy/haproxy.cfg        ← Configuração do load balancer
├── Dockerfile                     ← Build multi-stage do binário Go
└── go.mod / go.sum                ← Dependências Go (equivalente ao package.json)
```

### Convenções de Go que você precisa saber

- **`internal/`** → pacotes que só podem ser importados dentro deste projeto. É uma restrição da linguagem.
- **`pkg/`** → pacotes que podem ser importados por projetos externos.
- **`cmd/`** → contém os binários executáveis (cada subdiretório vira um binário).

---

## 4. Go para quem não conhece Go

Esta seção explica o mínimo de Go que você precisa para ler o código.

### Variáveis e tipos

```go
var nome string = "hello"    // declaração explícita
nome := "hello"              // declaração curta (Go infere o tipo)
```

### Funções

```go
func Soma(a int, b int) int {
    return a + b
}

// Múltiplos retornos (muito comum em Go):
func Dividir(a, b float64) (float64, error) {
    if b == 0 {
        return 0, fmt.Errorf("divisão por zero")
    }
    return a / b, nil
}

// Uso — erro é SEMPRE verificado:
resultado, err := Dividir(10, 0)
if err != nil {
    log.Fatal(err)
}
```

### Structs (equivalente a classes)

```go
type Bucket struct {
    ID             string `yaml:"id"`          // tag para deserialização YAML
    Host           string `yaml:"host"`
    MaxConnections int    `yaml:"max_connections"`
}

// Método — definido fora da struct, associado via receiver:
func (b *Bucket) Addr() string {
    return fmt.Sprintf("%s:%d", b.Host, b.Port)
}
```

### Interfaces (implícitas — sem "implements")

```go
type Reader interface {
    Read(p []byte) error
}

// Qualquer struct que tenha um método Read([]byte) error
// automaticamente implementa a interface Reader. Não precisa declarar nada.
```

### Goroutines (concorrência leve)

```go
go func() {
    // isso roda em paralelo, como uma thread leve
    fmt.Println("executando em background")
}()
```

### Channels (comunicação entre goroutines)

```go
ch := make(chan string, 10)  // canal com buffer de 10
ch <- "mensagem"             // envia
msg := <-ch                  // recebe (bloqueia se vazio)
```

### Select (aguarda múltiplos canais)

```go
select {
case msg := <-ch1:
    // ch1 recebeu primeiro
case <-ctx.Done():
    // contexto cancelado (timeout ou shutdown)
case <-time.After(5 * time.Second):
    // nenhum canal respondeu em 5s
}
```

### Context (propagação de cancelamento)

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()  // garante limpeza

// ctx é passado para todas as funções que precisam ser canceláveis.
// Quando cancel() é chamado, ctx.Done() é fechado e todas as goroutines que
// estão em <-ctx.Done() acordam e terminam.
```

### Defer (cleanup garantido)

```go
func processar() {
    f, _ := os.Open("arquivo.txt")
    defer f.Close()  // executa quando a função terminar, mesmo se der erro
    // ... usa f ...
}
```

### sync.WaitGroup (esperar goroutines terminarem)

```go
var wg sync.WaitGroup
wg.Add(3)          // vou lançar 3 goroutines
go func() {
    defer wg.Done() // marca uma como terminada
    // ... trabalho ...
}()
wg.Wait()          // bloqueia até todas terminarem
```

### sync.Mutex (exclusão mútua)

```go
var mu sync.Mutex
mu.Lock()
// acesso exclusivo à variável compartilhada
mu.Unlock()
```

---

## 5. O Entrypoint — cmd/proxy/main.go

Este é o ponto de partida. Quando o container Docker sobe, ele executa o binário compilado a partir deste arquivo. A função `main()` faz tudo na seguinte ordem:

### Sequência de inicialização

```
1. Parse flags       →  Lê --config e --buckets da linha de comando
2. config.Load()     →  Carrega proxy.yaml + buckets.yaml, valida, aplica defaults
3. Metrics HTTP      →  Sobe servidor HTTP :9090 com endpoint /metrics (Prometheus)
4. Health HTTP       →  Sobe servidor HTTP :8080 com endpoints /health, /health/live, /health/ready
5. pool.NewManager() →  Cria pools locais para cada bucket (abre min_idle conexões)
6. coordinator.New() →  Conecta ao Redis, carrega scripts Lua, registra instância
7. heartbeat.Start() →  Inicia goroutines de heartbeat (10s) e cleanup (30s)
8. queue.NewDistQ()  →  Cria a fila distribuída com circuit breaker
9. proxy.NewServer() →  Cria e inicia o TCP listener na porta 1433
10. Aguarda SIGTERM  →  Fica parado esperando sinal de shutdown
```

### Sequência de shutdown (reversa)

```
1. Recebe SIGINT ou SIGTERM
2. Para o proxy (fecha listener, espera sessões terminarem)
3. Para o heartbeat
4. Fecha o coordinator (remove instância do Redis)
5. Fecha o pool manager (fecha todas as conexões SQL)
6. Para o health check HTTP
7. Para o metrics HTTP
```

O shutdown é **gracioso**: conexões ativas terminam normalmente antes de fechar. O timeout de shutdown é 15 segundos.

### Flags de linha de comando

```bash
/app/proxy --config /app/configs/proxy.yaml --buckets /app/configs/buckets.yaml
```

---

## 6. Configuração — proxy.yaml e buckets.yaml

A configuração está dividida em dois arquivos por design: o `proxy.yaml` tem as configs que são iguais para todas as instâncias, enquanto o `buckets.yaml` define os bancos de dados.

### proxy.yaml

```yaml
proxy:
  listen_addr: "0.0.0.0"          # IP para bind (0.0.0.0 = todas interfaces)
  listen_port: 1433               # Porta TDS — mesma do SQL Server
  session_timeout: 5m             # Tempo máximo de uma sessão inteira
  idle_timeout: 60s               # Tempo sem atividade antes de dropar
  queue_timeout: 30s              # Quanto tempo esperar na fila por um slot
  max_queue_size: 1000            # Máximo de requisições na fila (circuit breaker)
  pinning_mode: "transaction"     # Modo de pinning de conexão
  health_check_port: 8080         # Porta do health check HTTP
  metrics_port: 9090              # Porta das métricas Prometheus

redis:
  addr: "redis:6379"              # Endereço do Redis
  pool_size: 20                   # Pool de conexões do client Redis
  dial_timeout: 5s
  read_timeout: 3s
  write_timeout: 3s
  heartbeat_interval: 10s         # Intervalo do heartbeat
  heartbeat_ttl: 30s              # TTL — se não renovar em 30s, é considerado morto

fallback:
  enabled: true                   # Ativa fallback local se Redis cair
  local_limit_divisor: 3          # Divide max_connections por 3 para limite local
```

### buckets.yaml

```yaml
buckets:
  - id: "bucket-1"
    host: "sqlserver-1"           # Hostname/IP do SQL Server
    port: 1433
    database: "AppDB_Bucket1"     # Database name (usado para roteamento)
    username: "sa"
    password: "YourStr0ngP@ssword1"
    max_connections: 50           # LIMITE GLOBAL (todas as instâncias somadas)
    min_idle: 5                   # Conexões pré-aquecidas no pool local
    max_idle_time: 5m             # Conexão ociosa por mais que isso é fechada
    connection_timeout: 30s       # Timeout para abrir conexão com SQL Server
    queue_timeout: 30s            # Timeout na fila para este bucket específico
```

### Defaults automáticos

Se você não definir um campo, ele ganha um valor default (definido em `config.go`):

| Campo | Default |
|---|---|
| `listen_addr` | `0.0.0.0` |
| `session_timeout` | 5 minutos |
| `idle_timeout` | 60 segundos |
| `queue_timeout` | 30 segundos |
| `max_queue_size` | 1000 |
| `pinning_mode` | `transaction` |
| `health_check_port` | 8080 |
| `metrics_port` | 9090 |
| `instance_id` | hostname do container |
| `redis.addr` | `redis:6379` |
| `redis.pool_size` | 20 |
| `heartbeat_interval` | 10 segundos |
| `heartbeat_ttl` | 30 segundos |
| `fallback.local_limit_divisor` | 3 |
| `min_idle` (por bucket) | 2 |
| `max_idle_time` (por bucket) | 5 minutos |
| `connection_timeout` (por bucket) | 30 segundos |

---

## 7. O que é um Bucket

**Bucket = um banco de dados SQL Server específico.** No contexto real, cada RDS tem um database, e cada database é um bucket.

A struct `Bucket` está em `pkg/bucket/bucket.go`:

```go
type Bucket struct {
    ID                string        // Identificador único (ex: "bucket-1")
    Host              string        // Hostname/IP do SQL Server
    Port              int           // Porta (normalmente 1433)
    Database          string        // Nome do database
    Username          string        // Usuário SQL
    Password          string        // Senha
    MaxConnections    int           // Limite global de conexões simultâneas
    MinIdle           int           // Conexões mínimas pré-aquecidas
    MaxIdleTime       time.Duration // Tempo máximo ocioso
    ConnectionTimeout time.Duration // Timeout para conectar
    QueueTimeout      time.Duration // Timeout na fila
}
```

Métodos importantes:
- **`DSN()`** → retorna a connection string `sqlserver://sa:senha@host:port?database=AppDB`
- **`Addr()`** → retorna `host:port` (usado para conexão TCP direta)

**No contexto real com 120 buckets**, o `buckets.yaml` terá 120 entradas, uma para cada RDS/database.

---

## 8. Fluxo de uma conexão passo a passo

Este é o fluxo mais importante para entender. Quando uma aplicação (CoreVM) quer falar com o SQL Server:

```
Aplicação                  HAProxy              Proxy (Go)                Redis              SQL Server
    │                         │                      │                      │                      │
    │── Abre conexão TCP ────▶│                      │                      │                      │
    │                         │── Conexão TCP ───────▶│                      │                      │
    │                         │                      │                      │                      │
    │                         │                      │◀── Aceita TCP ────────                      │
    │                         │                      │                      │                      │
    │── Pre-Login TDS ───────▶│─────────────────────▶│                      │                      │
    │                         │                      │                      │                      │
    │                         │                      │── pickBucket() ──────│                      │
    │                         │                      │   (lê hostname do    │                      │
    │                         │                      │    Pre-Login packet) │                      │
    │                         │                      │                      │                      │
    │                         │                      │── Acquire (Lua) ────▶│                      │
    │                         │                      │   "posso abrir       │                      │
    │                         │                      │    conexão no        │                      │
    │                         │                      │    bucket-1?"        │                      │
    │                         │                      │                      │                      │
    │                         │                      │◀── OK (ou espera) ───│                      │
    │                         │                      │                      │                      │
    │                         │                      │── net.DialTimeout ──────────────────────────▶│
    │                         │                      │   (abre TCP para SQL)│                      │
    │                         │                      │                      │                      │
    │                         │                      │── Forward Pre-Login ────────────────────────▶│
    │                         │                      │                      │                      │
    │                         │                      │◀── Pre-Login Response ──────────────────────│
    │◀─────────────────────────────────────────────── │                      │                      │
    │                         │                      │                      │                      │
    │══ io.Copy bidirecional (relay transparente) ══════════════════════════════════════════════════│
    │   Login7, queries, results, tudo passa direto  │                      │                      │
    │                         │                      │                      │                      │
    │── Fecha conexão ───────▶│─────────────────────▶│                      │                      │
    │                         │                      │── Release (Lua) ────▶│                      │
    │                         │                      │   "liberei conexão   │                      │
    │                         │                      │    do bucket-1"      │                      │
    │                         │                      │                      │                      │
```

### Detalhamento do handler.go (o arquivo mais importante)

O `handler.go` contém a struct `Session` — uma goroutine dedicada para cada conexão de cliente. Funciona assim:

1. **Lê o pacote Pre-Login** do cliente (primeiro pacote TDS, contém hostname)
2. **`pickBucket()`** — extrai o hostname do pacote TDS e encontra o bucket correspondente no config
3. **`dqueue.Acquire(bucketID)`** — pede permissão distribuída para abrir conexão:
   - Tenta caminho rápido (TryAcquire via Lua no Redis)
   - Se cheio, verifica circuit breaker (max_queue_size)
   - Se não estourou, espera na fila (Semaphore.Wait)
4. **`net.DialTimeout(bucket.Addr())`** — abre conexão TCP real com o SQL Server
5. **Forwards do Pre-Login** para o SQL Server e relays a resposta para o cliente
6. **`io.Copy` bidirecional** — duas goroutines fazem relay de bytes:
   - Uma copia cliente → SQL Server
   - Outra copia SQL Server → cliente
   - Relay é **100% transparente** — o proxy não interpreta os pacotes (exceto o Pre-Login)
7. **Quando qualquer lado fecha** a conexão, o relay para
8. **`cleanup()`** — libera o slot no Redis via `dqueue.Release()`

### O que acontece quando há erro

| Situação | Comportamento |
|---|---|
| Bucket não encontrado | Envia erro TDS 50002 ao cliente, fecha conexão |
| Fila de espera cheia (circuit breaker) | Envia erro TDS 50005 ao cliente, fecha conexão |
| Timeout na fila | Envia erro TDS 50004 ao cliente, fecha conexão |
| SQL Server não responde | Envia erro TDS 50003 ao cliente, fecha conexão |
| Erro durante relay | Conexão fecha naturalmente dos dois lados |

---

## 9. O protocolo TDS — o fio que conecta tudo

**TDS (Tabular Data Stream)** é o protocolo nativo do SQL Server. Tudo que o SSMS, sqlcmd, ou qualquer driver faz passa por TDS. O formato é binário.

### Como o proxy usa TDS

O proxy **não implementa o protocolo TDS inteiro**. Ele faz o mínimo necessário:

1. **Lê o pacote Pre-Login** (tipo `0x12`) — é o primeiro pacote que o cliente envia. Dele, o proxy extrai o **hostname do servidor** que o cliente quer atingir, para decidir qual bucket usar.

2. **Todo o resto é relay cego** — após o Pre-Login, o proxy simplesmente copia bytes entre o cliente e o SQL Server usando `io.Copy`. Login, queries, resultados, transações, tudo passa direto sem interpretação.

3. **Erros TDS** — quando o proxy precisa rejeitar uma conexão (fila cheia, timeout, etc.), ele constrói um pacote TDS de erro válido para que o cliente (SSMS, driver, etc.) entenda a mensagem de erro.

### Anatomia de um pacote TDS

```
┌─────────────────────────────────────┐
│ Header (8 bytes)                    │
│  - Type (1 byte): 0x12=Pre-Login   │
│  - Status (1 byte): 0x01=EOM       │
│  - Length (2 bytes): tamanho total  │
│  - SPID (2 bytes)                  │
│  - PacketID (1 byte)               │
│  - Window (1 byte)                 │
├─────────────────────────────────────┤
│ Payload (variável)                  │
│  - Conteúdo depende do tipo        │
└─────────────────────────────────────┘
```

O código que constrói erros TDS está em `internal/tds/error.go`. Ele monta o header + token ERROR (`0xAA`) + token DONE (`0xFD`).

---

## 10. Pool local de conexões

Cada instância do proxy mantém um **pool local** de conexões SQL Server para cada bucket. Isso é separado da coordenação distribuída.

### Estrutura

```
Manager (internal/pool/manager.go)
  └── BucketPool "bucket-1" (internal/pool/pool.go)
  │     ├── idle: [conn1, conn2, ...]       ← conexões disponíveis
  │     ├── active: {id3: conn3, ...}       ← conexões em uso
  │     └── waiters: [ch1, ch2, ...]        ← goroutines esperando
  └── BucketPool "bucket-2"
        └── ...
```

### Ciclo de vida de uma conexão no pool

```
Criação (sql.Open)
    ↓
  Idle (no slice idle)
    ↓  Acquire()
  Active (no map active)
    ↓  Release()
  Reset (sp_reset_connection)
    ↓
  Idle (volta pro slice)
    ↓  eviction (ociosa demais)
  Closed
```

### Manutenção automática

O pool roda uma goroutine de manutenção em background que:
- **Evicta conexões ociosas** que passaram de `max_idle_time`
- **Repõe conexões** para manter pelo menos `min_idle` disponíveis
- **Faz health check** (`SELECT 1`) nas conexões ociosas

### sp_reset_connection

Quando uma conexão é devolvida ao pool, o proxy executa `sp_reset_connection` no SQL Server. Isso limpa variáveis de sessão, tabelas temporárias, e outros estados, garantindo que o próximo cliente receba uma conexão "limpa".

### Nota importante

Na arquitetura atual, o pool local **existe mas o relay TDS não o usa diretamente para dados**. O relay usa conexão TCP direta (`net.Dial`). O pool serve para manter conexões SQL Server prontas e controlar limites locais. A coordenação de limites globais é feita pelo Redis (próxima seção).

---

## 11. Coordenação distribuída via Redis

Este é o coração da solução. Sem isso, cada instância do proxy controlaria apenas seus próprios limites, e o total global poderia ultrapassar o máximo.

### Chaves no Redis

```
proxy:bucket:bucket-1:count    →  "23"        ← Quantas conexões ativas globalmente
proxy:bucket:bucket-1:max      →  "50"        ← Limite máximo configurado
proxy:instance:abc123:conns    →  Hash {       ← Quantas conexões ESTA instância tem
                                     bucket-1: "8",
                                     bucket-2: "3"
                                   }
proxy:instance:abc123:heartbeat → "1"          ← Heartbeat com TTL de 30s
proxy:instances                →  Set {abc123, def456, ghi789}  ← Todas instâncias ativas
```

### Acquire — "posso abrir mais uma conexão?"

Quando o proxy precisa abrir uma conexão para o `bucket-1`, ele executa o script Lua `acquire.lua` no Redis:

```lua
-- Parâmetros:
--   KEYS[1] = proxy:bucket:bucket-1:count
--   KEYS[2] = proxy:bucket:bucket-1:max
--   KEYS[3] = proxy:instance:abc123:conns

local count = tonumber(redis.call("GET", KEYS[1]) or "0")
local max   = tonumber(redis.call("GET", KEYS[2]) or "0")

if max == 0 then return -2 end           -- max não configurado
if count >= max then return -1 end        -- sem vaga

redis.call("INCR", KEYS[1])              -- incrementa contador global
redis.call("HINCRBY", KEYS[3], ARGV[1], 1)  -- incrementa contador desta instância
return count + 1                          -- retorna novo total
```

**Por que Lua?** Porque é atômico. O Redis executa o script inteiro sem que outra instância interfira entre o `GET` e o `INCR`. Isso é essencial para evitar race conditions.

### Release — "liberei uma conexão"

Quando a conexão fecha, o proxy executa `release.lua`:

```lua
local count = tonumber(redis.call("GET", KEYS[1]) or "0")
if count <= 0 then return -1 end          -- proteção contra underflow

redis.call("DECR", KEYS[1])              -- decrementa contador global
redis.call("HINCRBY", KEYS[2], ARGV[1], -1)  -- decrementa desta instância
redis.call("PUBLISH", ARGV[2], "released")  -- notifica quem está esperando
return count - 1
```

### Pub/Sub — notificação em tempo real

Quando uma conexão é liberada, o `release.lua` faz `PUBLISH` no canal `proxy:release:bucket-1`. As instâncias que estão com goroutines esperando na fila recebem essa notificação imediatamente e tentam adquirir o slot.

### Semáforo (semaphore.go)

O semáforo combina duas estratégias para esperar por um slot:
1. **Pub/Sub** — inscreve no canal Redis e acorda quando alguém libera
2. **Polling** — a cada 500ms, tenta adquirir (fallback se o Pub/Sub falhar)

```go
select {
case <-pubSubCh:          // alguém liberou uma conexão!
    if TryAcquire() { return } // tenta pegar
case <-pollTicker.C:       // polling a cada 500ms
    if TryAcquire() { return }
case <-timer.C:            // timeout!
    return ErrTimeout
case <-ctx.Done():         // shutdown
    return ctx.Err()
}
```

---

## 12. Heartbeat e limpeza de instâncias mortas

### Problema

Se uma instância do proxy crashar sem fazer shutdown gracioso, ela pode deixar conexões contadas no Redis que nunca serão liberadas. Isso causaria um "leak" de slots.

### Solução: Heartbeat + Cleanup

Cada instância envia um heartbeat a cada 10 segundos:

```
SET proxy:instance:abc123:heartbeat "1" EX 30
```

O TTL de 30 segundos garante que, se a instância parar de enviar, a chave expira automaticamente.

### Cleanup de instâncias mortas

A cada 30 segundos, cada instância ativa verifica:

1. Lista todos os membros do set `proxy:instances`
2. Para cada instância, verifica se o heartbeat ainda existe (`EXISTS`)
3. Se não existe → instância está morta:
   - Lê o hash `proxy:instance:MORTA:conns` para saber quantas conexões ela tinha
   - Subtrai (`DECRBY`) esses valores dos contadores globais
   - Remove a instância do set `proxy:instances`
   - Deleta o hash de conexões

**Resultado:** os slots "fantasma" são recuperados automaticamente em no máximo ~30 segundos.

---

## 13. Fila de espera e Circuit Breaker

### Fila de espera (distributed.go)

Quando todas as conexões de um bucket estão em uso, novas requisições entram em uma fila de espera:

```
Requisição chega
       ↓
 TryAcquire() → Sucesso? → Abre conexão
       ↓ (falha)
 Verifica circuit breaker
       ↓
 fila < max_queue_size?
    SIM → Semaphore.Wait() → espera até timeout
    NÃO → Rejeita imediatamente com erro 50005
```

### Circuit breaker

O circuit breaker é simples mas eficaz: se a fila de espera ultrapassar `max_queue_size` (default: 1000), novas requisições são rejeitadas **imediatamente**. Isso evita que o sistema entre em colapso por acúmulo de requisições.

A rejeição é rápida — o cliente recebe erro TDS 50005 (QUEUE_FULL) e pode fazer retry ou routing para outro destino.

### Tipos de erro da fila

| Código | Nome | Significado |
|---|---|---|
| `QueueErrorTimeout` | Timeout na fila | Esperou `queue_timeout` sem conseguir slot |
| `QueueErrorFull` | Fila cheia | Circuit breaker ativado, rejeição imediata |

### Métricas da fila

- `proxy_queue_length` — profundidade atual da fila por bucket
- `proxy_queue_wait_duration_seconds` — quanto tempo cada requisição esperou

---

## 14. Modo Fallback — quando o Redis cai

Se o Redis ficar indisponível, o proxy não para. Ele entra em **modo fallback**:

### Como funciona

1. Uma operação no Redis falha
2. O proxy entra em modo fallback (`enterFallback()`)
3. O limite de conexões muda para: `max_connections / local_limit_divisor`
   - Com 3 instâncias e divisor=3: cada uma controla max/3 = 50/3 ≈ 16 conexões
4. A cada heartbeat tick, o proxy tenta sair do fallback (`ExitFallback()`)
5. Quando o Redis volta, reconcilia contadores e sai do fallback

### Por que dividir por 3?

Se existem N instâncias e o Redis caiu, nenhuma sabe o que as outras estão fazendo. A solução conservadora é: **cada uma assume que tem direito a max/N do total**. O divisor (configurável) é uma estimativa do número de instâncias.

### Configuração

```yaml
fallback:
  enabled: true
  local_limit_divisor: 3   # Para 120 buckets reais, ajustar conforme N de instâncias
```

---

## 15. Health Check e Métricas

### Health Check (porta 8080)

| Endpoint | Propósito |
|---|---|
| `GET /health/live` | **Liveness probe** — responde `200` se o processo está vivo. Usado pelo HAProxy/NLB para saber se o proxy existe. |
| `GET /health/ready` | **Readiness probe** — testa conexão com Redis e todos os SQL Servers. Responde `200` se tudo ok, `503` se algo falhou. |
| `GET /health` | Igual ao ready, retorna relatório detalhado com latência de cada componente. |

Exemplo de resposta do `/health`:
```json
{
  "status": "healthy",
  "timestamp": "2025-01-15T10:30:00Z",
  "instance_id": "proxy-1",
  "components": [
    {"name": "redis", "status": "healthy", "latency": "1.2ms"},
    {"name": "sqlserver-bucket-1", "status": "healthy", "latency": "5.3ms"},
    {"name": "sqlserver-bucket-2", "status": "healthy", "latency": "4.8ms"}
  ]
}
```

### Métricas Prometheus (porta 9090)

Todas as métricas têm prefixo `proxy_` e estão definidas em `internal/metrics/metrics.go`:

| Métrica | Tipo | Labels | O que mede |
|---|---|---|---|
| `proxy_connections_active` | Gauge | bucket | Conexões ativas agora |
| `proxy_connections_idle` | Gauge | bucket | Conexões ociosas no pool |
| `proxy_connections_pinned` | Gauge | bucket | Conexões pinadas (em transação) |
| `proxy_connections_max` | Gauge | bucket | Limite configurado |
| `proxy_connections_total` | Counter | bucket, status | Total de conexões (acquired, released, timeout, etc.) |
| `proxy_queue_length` | Gauge | bucket | Profundidade da fila |
| `proxy_queue_wait_duration_seconds` | Histogram | bucket | Tempo de espera na fila |
| `proxy_connection_errors_total` | Counter | bucket, reason | Erros (create_failed, reset_failed, etc.) |
| `proxy_redis_operations_total` | Counter | operation, status | Operações Redis (acquire, release, etc.) |
| `proxy_instance_heartbeat` | Gauge | instance | Heartbeat ativo |

O Grafana (porta 3001) está pré-configurado para consumir essas métricas via Prometheus (porta 9091).

---

## 16. Infraestrutura Docker

O `docker-compose.yml` define 11 containers:

### Containers

| Container | Imagem | Porta host | Papel |
|---|---|---|---|
| `sqlserver-1` | mcr.microsoft.com/mssql/server:2022 | 14331 | Banco bucket-1 |
| `sqlserver-2` | mcr.microsoft.com/mssql/server:2022 | 14332 | Banco bucket-2 |
| `sqlserver-3` | mcr.microsoft.com/mssql/server:2022 | 14333 | Banco bucket-3 |
| `init-db` | mcr.microsoft.com/mssql-tools:latest | — | Cria databases e tabelas iniciais |
| `redis` | redis:7.4 | 6379 | Coordenação distribuída |
| `proxy-1` | Build local (Dockerfile) | — | Instância do proxy |
| `proxy-2` | Build local (Dockerfile) | — | Instância do proxy |
| `proxy-3` | Build local (Dockerfile) | — | Instância do proxy |
| `haproxy` | haproxy:3.1 | 1433 | Load balancer TCP |
| `prometheus` | prom/prometheus | 9091 | Coleta métricas |
| `grafana` | grafana/grafana | 3001 | Dashboards |

### Rede

Todos os containers estão na rede bridge `proxy-network`. Isso permite que se referenciem pelo nome (ex: `redis:6379`, `sqlserver-1:1433`).

### Como subir

```bash
cd deployments
docker compose up -d --build
```

### Como testar

```bash
# Via sqlcmd (conecta via HAProxy → Proxy → SQL Server)
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword1' -d AppDB_Bucket1 -Q "SELECT 1"
```

---

## 17. Erros TDS enviados ao cliente

Quando o proxy rejeita uma conexão, ele envia um pacote TDS de erro válido. O cliente (SSMS, driver .NET, driver JDBC) exibe a mensagem como se fosse um erro do SQL Server.

| Código | Severidade | Mensagem | Quando acontece |
|---|---|---|---|
| 50000 | 16 | Internal proxy error | Erro genérico do proxy |
| 50001 | 16 | Connection pool exhausted | Pool local cheio (todas conexões em uso) |
| 50002 | 16 | Unable to route connection | Bucket não encontrado para o hostname/database |
| 50003 | 16 | Backend server unavailable | SQL Server não respondeu ao connect |
| 50004 | 16 | Queue timeout | Esperou `queue_timeout` e não conseguiu slot |
| 50005 | 16 | Queue full — circuit breaker open | Fila tem mais de `max_queue_size` requisições |

Os códigos 50000+ estão na faixa de "user-defined messages" do SQL Server, então não conflitam com mensagens nativas.

---

## 18. Shutdown gracioso

O shutdown é disparado por `SIGINT` (Ctrl+C) ou `SIGTERM` (Docker stop). O processo é:

1. **Proxy listener** — para de aceitar novas conexões
2. **Sessões ativas** — cada sessão recebe cancelamento via context e tem chance de terminar
3. **WaitGroup** — main espera todas as sessões terminarem (até 15 segundos)
4. **Heartbeat** — para de enviar heartbeats
5. **Coordinator** — remove a instância do Redis e fecha conexão
6. **Pool manager** — fecha todas as conexões SQL Server
7. **HTTP servers** — fecha health check e métricas

Se alguma sessão não terminar em 15 segundos, o `context.WithTimeout` do shutdown cancela tudo forçadamente.

---

## 19. Mapa de arquivos — referência rápida

| Arquivo | Linhas | Complexidade | O que faz |
|---|---|---|---|
| `cmd/proxy/main.go` | ~185 | Baixa | Monta tudo e faz shutdown. **Leia primeiro.** |
| `internal/proxy/handler.go` | ~322 | **Alta** | Lógica de uma sessão TDS. **O mais importante.** |
| `internal/proxy/listener.go` | ~165 | Média | TCP accept loop e graceful stop |
| `internal/coordinator/redis.go` | ~480 | **Alta** | Toda lógica Redis, Lua, fallback |
| `internal/coordinator/semaphore.go` | ~136 | Média | Wait com Pub/Sub + polling |
| `internal/coordinator/heartbeat.go` | ~197 | Média | Heartbeat + cleanup de mortos |
| `internal/queue/distributed.go` | ~207 | Média | Fila + circuit breaker |
| `internal/pool/pool.go` | ~444 | **Alta** | Pool completo com waiters e eviction |
| `internal/pool/manager.go` | ~135 | Baixa | Manager simples sobre BucketPool |
| `internal/pool/connection.go` | ~176 | Baixa | Struct PooledConn com metadados |
| `internal/config/config.go` | ~235 | Baixa | Parse YAML + validation + defaults |
| `internal/tds/error.go` | ~210 | Média | Constrói pacotes TDS de erro binários |
| `internal/metrics/metrics.go` | ~93 | Baixa | Definição de métricas Prometheus |
| `internal/health/health.go` | ~266 | Baixa | HTTP health check endpoints |
| `pkg/bucket/bucket.go` | ~49 | Baixa | Struct Bucket (DSN, Addr) |
| `coordinator/lua/acquire.lua` | ~35 | Média | Script Lua atômico de aquisição |
| `coordinator/lua/release.lua` | ~38 | Média | Script Lua atômico de liberação |
| `configs/proxy.yaml` | ~30 | Baixa | Configuração do proxy |
| `configs/buckets.yaml` | ~30 | Baixa | Configuração dos buckets |
| `deployments/docker-compose.yml` | ~278 | Média | Toda a infraestrutura Docker |
| `Dockerfile` | ~30 | Baixa | Build multi-stage Go |

### Ordem recomendada de leitura

1. `cmd/proxy/main.go` — como tudo começa
2. `configs/proxy.yaml` + `configs/buckets.yaml` — o que é configurável
3. `internal/proxy/handler.go` — o fluxo de uma conexão
4. `internal/coordinator/redis.go` — como a coordenação funciona
5. `internal/coordinator/lua/acquire.lua` e `release.lua` — a atomicidade
6. `internal/queue/distributed.go` — fila e circuit breaker
7. `internal/coordinator/semaphore.go` — como esperar por slots
8. `internal/coordinator/heartbeat.go` — resiliência a crashes
9. `internal/tds/error.go` — erros para o cliente
10. O resto conforme necessidade

---

## 20. Aplicando ao contexto real com 120 buckets

### O que muda no buckets.yaml

O arquivo `buckets.yaml` terá 120 entradas:

```yaml
buckets:
  - id: "rds-cliente-001"
    host: "rds-cliente-001.abc123.us-east-1.rds.amazonaws.com"
    port: 1433
    database: "AppDB"
    username: "app_user"
    password: "..."
    max_connections: 50
    min_idle: 2

  - id: "rds-cliente-002"
    host: "rds-cliente-002.abc123.us-east-1.rds.amazonaws.com"
    port: 1433
    database: "AppDB"
    username: "app_user"
    password: "..."
    max_connections: 50
    min_idle: 2

  # ... até rds-cliente-120
```

### O que muda no proxy.yaml

```yaml
proxy:
  listen_addr: "0.0.0.0"
  listen_port: 1433
  max_queue_size: 5000          # Aumentar — 120 buckets geram mais tráfego
  queue_timeout: 30s

redis:
  addr: "redis-cluster:6379"    # Redis em produção (ElastiCache, etc.)
  pool_size: 50                 # Mais conexões Redis para 120 buckets

fallback:
  enabled: true
  local_limit_divisor: 3        # Ajustar para número real de instâncias do proxy
```

### Chaves Redis — escala

Com 120 buckets e 3 instâncias de proxy:
- **240 chaves** de contadores (`count` e `max` por bucket)
- **3 hashes** de instância (um por proxy)
- **3 chaves** de heartbeat
- **1 set** de instâncias
- **120 canais** Pub/Sub (um por bucket)

Total: ~367 chaves. Redis lida tranquilamente com isso.

### Considerações de performance

| Aspecto | POC (3 buckets) | Produção (120 buckets) |
|---|---|---|
| Memória Redis | Desprezível | Ainda desprezível (~50KB) |
| Lua scripts | 1 EVALSHA por Acquire/Release | Mesmo — O(1) por operação |
| Pub/Sub canais | 3 | 120 — Redis suporta milhões |
| Startup time | <1s | ~5-10s (120 pools × min_idle) |
| Config parsing | Instantâneo | Instantâneo |
| Health check | 3 SQL Servers | 120 SQL Servers em paralelo (goroutines) |

### Pontos de atenção para produção

1. **Redis em HA** — Use ElastiCache com replica ou Redis Sentinel. Se Redis cai, o fallback funciona mas é limitado.

2. **`local_limit_divisor`** — Deve ser ≥ ao número de instâncias do proxy. Se tem 5 instâncias, use 5.

3. **`min_idle` por bucket** — Com 120 buckets e `min_idle=5`, cada instância abriria 600 conexões no startup. Considere `min_idle=1` ou `min_idle=0` para produção.

4. **`max_connections` por bucket** — Esse é o limite GLOBAL. Se o RDS suporta 100 conexões e você quer margem, configure 80.

5. **HAProxy → NLB** — Em AWS, substitua o HAProxy por um Network Load Balancer (L4 TCP). O proxy já suporta health checks HTTP.

6. **`max_queue_size`** — Com 120 buckets, pode haver mais requisições simultâneas. Aumente conforme carga esperada.

7. **Secrets** — As senhas dos buckets estão em texto plano no YAML. Para produção, use AWS Secrets Manager ou variáveis de ambiente.

8. **Monitoramento** — Configure alertas no Grafana para:
   - `proxy_queue_length > 100` (fila crescendo)
   - `proxy_connection_errors_total` crescendo (problemas de conexão)
   - `proxy_instance_heartbeat` sumindo (instância morta)

9. **Routing** — O `pickBucket()` atualmente usa o hostname do Pre-Login TDS. Certifique-se de que as aplicações estão configuradas com o hostname/database correto para cada bucket.

10. **Scale-out** — Para adicionar mais instâncias do proxy, basta subir mais containers com o mesmo config. O Redis coordena automaticamente.

---

## Glossário

| Termo | Significado |
|---|---|
| **Bucket** | Um banco de dados SQL Server específico com seu próprio limite de conexões |
| **Slot** | Uma "vaga" de conexão disponível para um bucket |
| **Acquire** | Pedir permissão para abrir uma conexão (reservar um slot) |
| **Release** | Devolver um slot após fechar a conexão |
| **TDS** | Tabular Data Stream — protocolo binário do SQL Server |
| **Pre-Login** | Primeiro pacote TDS enviado pelo cliente, contém hostname do servidor |
| **Relay** | Copiar bytes entre cliente e servidor de forma transparente |
| **Pinning** | Fixar uma conexão a um cliente (ex: durante transação) |
| **Circuit Breaker** | Mecanismo que rejeita requisições quando a fila está muito grande |
| **Fallback** | Modo degradado quando Redis está indisponível (controle local) |
| **Heartbeat** | Sinal periódico que prova que a instância está viva |
| **Eviction** | Remoção de conexões ociosas do pool |
| **Goroutine** | Thread leve do Go (milhares podem rodar simultaneamente) |
| **Channel** | Mecanismo de comunicação entre goroutines |
| **Context** | Objeto que propaga cancelamento e timeouts através da call stack |
| **EVALSHA** | Comando Redis que executa script Lua pré-carregado (atômico) |
| **Pub/Sub** | Padrão publicar/assinar do Redis para notificações em tempo real |
