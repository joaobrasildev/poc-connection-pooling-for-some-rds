# Entendimento Técnico — Proxy de Connection Pooling para RDS SQL Server

## 1. Contexto do Problema

### 1.1 Arquitetura Atual

| Componente | Descrição |
|---|---|
| **Aplicativos (Tenants)** | ~300.000 aplicativos que atuam como tenants isolados na base de dados |
| **Buckets** | ~120 buckets lógicos que agrupam os tenants |
| **RDS Instances** | 1 instância RDS (**SQL Server**) por bucket → ~120 instâncias RDS |
| **Linguagem Core** | ~90% da aplicação escrita em Harbour (derivado de xBase/Clipper) |
| **Runtime** | Cada aplicativo, ao ser aberto, instancia uma **coreVM** independente |
| **Modelo de Conexão** | Cada coreVM abre uma conexão **TDS direta** com o RDS do bucket correspondente — sem pooling, sem controle de concorrência |
| **Rate Limit Existente** | Limitador de ~10 chamadas sequenciais por minuto por tenant (já existente, externo ao proxy) |

### 1.2 Fluxo Atual (Simplificado)

```
┌──────────┐     ┌──────────┐     ┌──────────┐
│  App A1  │     │  App A2  │     │  App An  │     (300k apps)
│ (coreVM) │     │ (coreVM) │     │ (coreVM) │
└────┬─────┘     └────┬─────┘     └────┬─────┘
     │                │                │
     │  Conexão       │  Conexão       │  Conexão
     │  TDS (1433)    │  TDS (1433)    │  TDS (1433)
     │                │                │
     ▼                ▼                ▼
┌─────────────────────────────────────────────┐
│         RDS SQL Server (Bucket X)           │
│         Sem limite de conexões              │
│         controlado pela aplicação           │
└─────────────────────────────────────────────┘
```

### 1.3 Problema Principal

- **Esgotamento de memória nos RDS SQL Server** causado, entre outros fatores, pela quantidade descontrolada de conexões simultâneas.
- Cada conexão no SQL Server consome memória (memory grants, buffers, worker threads — tipicamente entre **5MB e 15MB por conexão**, podendo chegar a mais dependendo das queries).
- SQL Server tem um limite de **worker threads** (configurável, default ~512 para instâncias padrão). Cada conexão ativa consome um worker thread enquanto executa uma query.
- Com 300k apps e ~120 RDS, o cenário worst-case é de ~2.500 conexões simultâneas por RDS (300k / 120), mas picos podem ser muito maiores dependendo da distribuição de carga.
- **Não há nenhum mecanismo de controle** entre a coreVM e o RDS — cada coreVM abre e mantém sua própria conexão de forma independente.

---

## 2. Solução Proposta

### 2.1 Proxy de Controle de Conexões (Connection Pooling Proxy)

Um serviço intermediário em **Go (Golang)** que se posiciona entre as coreVMs e os RDS SQL Server, atuando como:

1. **Centralizador** — Todas as conexões de todos os buckets passam pelo proxy.
2. **Controlador de concorrência** — Limita o número máximo de conexões simultâneas por RDS/bucket.
3. **Gerenciador de fila** — Quando o limite de conexões de um bucket é atingido, requisições são enfileiradas e aguardam liberação (backpressure com fila de espera).
4. **Pool reutilizável** — Mantém um pool de conexões pré-estabelecidas com cada RDS, reutilizando-as entre requisições.

### 2.2 Fluxo Proposto

```
┌──────────┐  ┌──────────┐  ┌──────────┐
│  App A1  │  │  App A2  │  │  App An  │   (300k apps)
│ (coreVM) │  │ (coreVM) │  │ (coreVM) │
└────┬─────┘  └────┬─────┘  └────┬─────┘
     │             │             │
     │  TDS/TCP    │  TDS/TCP    │  TDS/TCP
     │  (1433)     │  (1433)     │  (1433)
     ▼             ▼             ▼
┌─────────────────────────────────────────┐
│     CONNECTION POOLING PROXY (Go)       │
│     Protocolo: TDS (Tabular Data        │
│     Stream) — Transparente para coreVM  │
│                                         │
│  ┌─────────────────────────────────┐    │
│  │  Instância 1  │  Instância 2   │    │   ◄── Escalamento Horizontal
│  │  Instância 3  │  Instância N   │    │
│  └─────────┬─────┴────────┬───────┘    │
│            │               │            │
│  ┌─────────▼───────────────▼────────┐   │
│  │       Estado Compartilhado       │   │
│  │           (Redis Cluster)        │   │
│  │  • Contadores atômicos por RDS   │   │
│  │  • Fila de espera por bucket     │   │
│  └──────────────────────────────────┘   │
└────────┬──────────┬──────────┬──────────┘
         │          │          │
         ▼          ▼          ▼
    ┌─────────┐ ┌─────────┐ ┌─────────┐
    │SQL Srv 1│ │SQL Srv 2│ │SQL S.120│    (~120 RDS SQL Server)
    └─────────┘ └─────────┘ └─────────┘
```

### 2.3 Protocolo de Comunicação (coreVM ↔ Proxy)

**Decisão: Wire Protocol TDS (Tabular Data Stream) — Zero mudança na coreVM.**

A coreVM **não será modificada**. O proxy implementa o **protocolo TDS**, sendo completamente transparente. A coreVM se conecta no proxy exatamente como se conectaria no SQL Server real — a única mudança é o **host/IP** de destino (que pode ser feito via configuração/DNS sem tocar no código).

O protocolo TDS (Tabular Data Stream) é o wire protocol nativo do Microsoft SQL Server:

| Aspecto | TDS (SQL Server) |
|---|---|
| **Porta padrão** | 1433 |
| **Handshake** | Pre-login → Login7 → Login Response |
| **Encryption** | TLS opcional/obrigatório (negociado no pre-login) |
| **Pacotes** | Framed: Header (8 bytes) + Data |
| **Tipos de pacote** | SQL Batch, RPC (prepared stmts), Bulk Load, Attention, Transaction Manager |
| **Prepared Statements** | Via pacotes RPC (`sp_prepare` / `sp_execute` / `sp_unprepare`) |
| **Transactions** | Via Transaction Manager Request ou T-SQL explícito (`BEGIN TRAN`) |

### 2.4 Como o Proxy Lida com o Payload

**O proxy recebe e encaminha TODO o payload — tanto das requisições (queries) quanto das respostas (resultsets).** Porém, funciona como um **relay/pipe transparente**, não como um intermediário que interpreta os dados:

```
┌────────┐                ┌───────┐               ┌────────────┐
│ coreVM │ ── TDS Req ──► │ PROXY │ ── TDS Req ──►│ SQL Server │
│        │ ◄─ TDS Res ──  │       │ ◄─ TDS Res ── │            │
└────────┘                └───────┘               └────────────┘

O proxy NÃO interpreta o conteúdo — apenas lê headers para roteamento.
```

**O que o proxy FAZ:**
- Recebe os bytes brutos do pacote TDS do cliente
- Lê o **header TDS** (8 bytes) para identificar o tipo de pacote e tamanho
- Encaminha (forward) os bytes para o SQL Server backend
- Recebe a resposta do SQL Server e encaminha de volta ao cliente

**O que o proxy precisa entender (mínimo necessário):**
- **Pre-Login + Login7**: Para extrair o database name e rotear para o bucket correto
- **Tipo de pacote TDS**: Para detectar início/fim de transações e prepared statements (connection pinning)
- **Attention Signal**: Para cancelamento de queries
- **Tamanho do pacote**: Para saber quando um pacote TDS está completo

**O que o proxy NÃO faz:**
- ❌ Não parseia SQL
- ❌ Não entende resultsets/colunas
- ❌ Não converte para JSON ou qualquer outro formato
- ❌ Não serializa/deserializa dados
- ❌ Não cacheia resultados

> **Analogia:** O proxy funciona como um **roteador inteligente** — ele olha o "envelope" (header TDS) para decidir pra onde mandar, mas não abre a "carta" (conteúdo SQL/resultset). Isso mantém a latência extremamente baixa.

### 2.5 Connection Pinning (Prepared Statements, Transactions e DDL)

Como a coreVM usa **tanto queries preparadas quanto não preparadas**, e precisa suportar **DDL e stored procedures**, o proxy implementa **connection pinning**:

| Cenário | Comportamento |
|---|---|
| **Query simples (SQL Batch sem transação)** | Pode usar qualquer conexão do pool — sem pinning |
| **Prepared Statement (`sp_prepare`)** | Conexão "pinada" ao cliente até o `sp_unprepare` — o handle é válido apenas naquela conexão |
| **Transaction (`BEGIN TRAN`)** | Conexão "pinada" ao cliente até `COMMIT` ou `ROLLBACK` |
| **DDL (`CREATE TABLE`, `ALTER`, etc.)** | Executado como SQL Batch — sem pinning extra necessário |
| **Stored Procedure (`EXEC`)** | Fora de transação: qualquer conexão; dentro de transação: segue o pinning da transação |
| **Session State (`SET` commands)** | Se a coreVM altera session settings, a conexão precisa ser pinada ou o proxy precisa replicar o estado |

**Estratégia de Pooling:**

```
Modo "Transaction Pooling" (implementado na POC):
─────────────────────────────────────────────────
• Conexão é adquirida do pool no início de uma transação ou prepared statement
• Conexão é devolvida ao pool no COMMIT/ROLLBACK ou sp_unprepare
• Para queries simples fora de transação: acquire → execute → release (imediato)
• Maximiza a reutilização de conexões

Modo "Session Pooling" (fallback se necessário):
────────────────────────────────────────────────
• Conexão é adquirida quando o cliente conecta
• Conexão é devolvida quando o cliente desconecta
• Menor eficiência, mas mais seguro para session state complexo
```

### 2.6 Escalamento Horizontal

- Múltiplas instâncias do proxy rodando atrás de um **Load Balancer (NLB — Network Load Balancer)**.
- Cada instância precisa ter visão global do estado de conexões de **todos os buckets**.
- **Redis** é utilizado como store de estado compartilhado entre instâncias:
  - **Contadores atômicos** (`INCR`/`DECR`) por bucket para tracking de conexões ativas.
  - **Operações atômicas com Lua scripts** para garantir check-and-increment sem race conditions.
  - **Pub/Sub ou Streams** para notificar instâncias quando conexões são liberadas (despertar filas de espera).

### 2.7 Fila de Espera (Backpressure)

Quando um bucket atinge o limite máximo de conexões simultâneas:

1. A requisição **não é rejeitada** — é enfileirada.
2. A coreVM fica em espera (blocking) até que uma conexão esteja disponível.
3. Timeout configurável para evitar espera infinita.
4. Priorização FIFO padrão.
5. Respeita o rate limit existente de ~10 chamadas/minuto por tenant (o proxy não duplica esse controle, mas pode usar a informação para priorização futura).

### 2.8 Requisitos Não-Funcionais

| Requisito | Detalhamento |
|---|---|
| **Alta Disponibilidade** | O proxy não pode ser single point of failure. Múltiplas instâncias, health checks, failover automático. |
| **Baixa Latência** | O overhead introduzido pelo proxy deve ser mínimo (< 1-2ms por hop em condições normais). |
| **Escalabilidade** | Suporte a 300k tenants com escalamento horizontal. |
| **Transparência** | Zero mudança na coreVM — proxy é drop-in replacement (apenas mudança de host/IP). |
| **Observabilidade** | Métricas de conexões ativas por bucket, tempo em fila, latência do proxy, uso de memória. |
| **Resiliência** | Se o Redis ficar indisponível, o proxy deve ter fallback (modo degradado com controle local). |
| **Suporte SQL Completo** | DDL, DML, stored procedures, prepared statements, transactions — tudo via TDS. |

---

## 3. Componentes Técnicos

### 3.1 Stack Tecnológica

| Componente | Tecnologia |
|---|---|
| **Proxy** | Go 1.22+ |
| **Estado Compartilhado** | Redis 7+ (Cluster mode para HA) |
| **Protocolo** | TDS (Tabular Data Stream) — wire protocol nativo do SQL Server |
| **Containerização** | Docker + Docker Compose |
| **Simulação RDS** | `mcr.microsoft.com/mssql/server:2022-latest` (SQL Server Linux em container) |
| **Load Balancer** | HAProxy (L4) no Docker Compose para simular NLB |
| **Métricas** | Prometheus + Grafana |
| **Driver Go ↔ SQL Server** | `github.com/microsoft/go-mssqldb` (para conexões backend do pool) |
| **TDS Protocol** | Implementação custom do subset TDS necessário para proxy (handshake + packet relay) |

### 3.2 Estrutura de Dados no Redis

```
# Conexões ativas por bucket (contador atômico)
bucket:{bucket_id}:active_connections  →  integer

# Limite máximo por bucket
bucket:{bucket_id}:max_connections     →  integer

# Metadados do bucket (endpoint RDS, porta, etc.)
bucket:{bucket_id}:config             →  hash {host, port, db_name, ...}

# Canal Pub/Sub para notificação de liberação
channel:bucket:{bucket_id}:released   →  pub/sub
```

### 3.3 Algoritmo de Controle (Pseudocódigo)

```
FUNCTION acquire_connection(bucket_id, timeout):
    LOOP until timeout:
        current = REDIS.GET("bucket:{bucket_id}:active_connections")
        max     = REDIS.GET("bucket:{bucket_id}:max_connections")

        IF current < max:
            // Lua script atômico: check-and-increment
            success = REDIS.EVAL(lua_atomic_acquire, bucket_id)
            IF success:
                conn = POOL.get_or_create(bucket_id)
                RETURN conn
        
        // Limite atingido — aguarda notificação via Pub/Sub
        WAIT on channel "bucket:{bucket_id}:released" with timeout
    
    RETURN error("timeout waiting for connection")


FUNCTION release_connection(bucket_id, conn):
    POOL.return(bucket_id, conn)
    REDIS.DECR("bucket:{bucket_id}:active_connections")
    REDIS.PUBLISH("bucket:{bucket_id}:released", "1")
```

### 3.4 Detecção de Connection Pinning (TDS Packet Inspection)

```
FUNCTION on_client_packet(packet):
    type = packet.header.type
    
    SWITCH type:
        CASE TDS_SQL_BATCH (0x01):
            // Inspecionar se contém BEGIN TRAN / COMMIT / ROLLBACK
            IF contains_begin_transaction(packet):
                session.pinned = true
                session.in_transaction = true
            IF contains_commit_or_rollback(packet):
                session.in_transaction = false
                IF session.prepared_count == 0:
                    session.pinned = false
                    RELEASE connection back to pool
        
        CASE TDS_RPC (0x03):
            // Prepared statements via sp_prepare/sp_execute
            proc_name = extract_rpc_proc_name(packet)
            IF proc_name == "sp_prepare":
                session.pinned = true
                session.prepared_count++
            IF proc_name == "sp_unprepare":
                session.prepared_count--
                IF session.prepared_count == 0 AND NOT session.in_transaction:
                    session.pinned = false
                    RELEASE connection back to pool
        
        CASE TDS_TRANSACTION_MANAGER (0x0E):
            // Transaction begin/commit/rollback via TM Request
            IF action == BEGIN:
                session.pinned = true
                session.in_transaction = true
            IF action == COMMIT or ROLLBACK:
                session.in_transaction = false
                IF session.prepared_count == 0:
                    session.pinned = false
                    RELEASE connection back to pool
    
    FORWARD packet to backend
```

---

## 4. Premissas Validadas

| # | Questão | Resposta |
|---|---|---|
| 1 | Engine do RDS | **SQL Server** (TDS protocol, porta 1433) |
| 2 | Modificação na coreVM | **Não** — proxy é transparente, apenas mudança de host/IP |
| 3 | Prepared statements | **Ambos** — queries preparadas e não preparadas. Requer connection pinning |
| 4 | Tempo médio de conexão | **A definir** — POC usará padrão de 5 minutos por sessão com idle timeout de 60s |
| 5 | Autenticação coreVM ↔ RDS | **Provavelmente sem mecanismo especial** — proxy faz forward transparente do Login7 TDS |
| 6 | Tipos de operação | **Completo** — DDL, DML, stored procedures, prepared statements, transactions |
| 7 | Distribuição de apps por bucket | **Relativamente uniforme** com variações entre buckets |
| 8 | Controle por tenant | **Não no proxy** — já existe rate limit externo de ~10 chamadas sequenciais/minuto por tenant |
