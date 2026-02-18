# Processo de Execução — POC Connection Pooling Proxy

> Guia completo com todos os comandos para compilar, subir, testar e operar o projeto.
> Execute os comandos na ordem apresentada.

---

## Índice

1. [Pré-requisitos](#1-pré-requisitos)
2. [Clonar e preparar o projeto](#2-clonar-e-preparar-o-projeto)
3. [Build local (Go)](#3-build-local-go)
4. [Subir toda a infraestrutura](#4-subir-toda-a-infraestrutura)
5. [Verificar se os containers estão saudáveis](#5-verificar-se-os-containers-estão-saudáveis)
6. [Smoke tests — conectividade básica](#6-smoke-tests--conectividade-básica)
7. [Testes de operações de dados](#7-testes-de-operações-de-dados)
8. [Testes de transação](#8-testes-de-transação)
9. [Verificar health checks](#9-verificar-health-checks)
10. [Verificar métricas Prometheus](#10-verificar-métricas-prometheus)
11. [Verificar Redis (coordenação distribuída)](#11-verificar-redis-coordenação-distribuída)
12. [Testes de carga — conexões concorrentes](#12-testes-de-carga--conexões-concorrentes)
13. [Teste de Queue Timeout (erro 50004)](#13-teste-de-queue-timeout-erro-50004)
14. [Teste de Circuit Breaker (erro 50005)](#14-teste-de-circuit-breaker-erro-50005)
15. [Teste de Circuit Breaker via proxy direto](#15-teste-de-circuit-breaker-via-proxy-direto)
16. [Teste de cenários de erro combinados](#16-teste-de-cenários-de-erro-combinados)
17. [Verificar logs do proxy](#17-verificar-logs-do-proxy)
18. [Monitoramento — Grafana e HAProxy Stats](#18-monitoramento--grafana-e-haproxy-stats)
19. [Testes de resiliência](#19-testes-de-resiliência)
20. [Parar e limpar tudo](#20-parar-e-limpar-tudo)
21. [Comandos úteis do Makefile](#21-comandos-úteis-do-makefile)

---

## 1. Pré-requisitos

Antes de começar, confirme que as seguintes ferramentas estão instaladas:

```bash
# Go 1.24+
go version

# Docker e Docker Compose
docker --version
docker compose version

# sqlcmd (para testes via linha de comando)
# macOS:
brew install sqlcmd
# Ou via Microsoft:
# https://learn.microsoft.com/en-us/sql/tools/sqlcmd/sqlcmd-utility

# redis-cli (opcional, para inspecionar Redis)
# macOS:
brew install redis

# curl (geralmente já instalado)
curl --version

# jq (opcional, para formatar saídas JSON)
brew install jq
```

---

## 2. Clonar e preparar o projeto

```bash
# Entrar no diretório do projeto
cd poc-connection-pooling-for-some-rds

# Baixar dependências Go
go mod download
go mod tidy
```

**O que faz:** Baixa todas as bibliotecas Go necessárias (go-redis, go-mssqldb, prometheus client, yaml parser) e atualiza os arquivos `go.mod` e `go.sum`.

---

## 3. Build local (Go)

```bash
# Compilar o binário do proxy
go build -o bin/proxy ./cmd/proxy/
```

**O que faz:** Compila todo o código Go em um único binário executável `bin/proxy`. Valida que não há erros de compilação.

```bash
# Verificar que compilou corretamente
ls -la bin/proxy
```

```bash
# (Opcional) Rodar vet para detectar problemas no código
go vet ./...
```

**O que faz:** Análise estática do código — detecta erros comuns como variáveis não usadas, chamadas de printf mal formatadas, etc.

---

## 4. Subir toda a infraestrutura

```bash
# Entrar no diretório de deployments
cd deployments

# Build das imagens Docker + subir todos os 11 containers
docker compose up -d --build
```

**O que faz:** Executa em sequência:
1. Build multi-stage do proxy Go (`Dockerfile`)
2. Sobe 3 SQL Servers (bucket-1, bucket-2, bucket-3)
3. Sobe Redis
4. Aguarda SQL Servers ficarem healthy, depois roda `init-db` (cria databases + tabelas + seed data)
5. Sobe 3 instâncias do proxy (proxy-1, proxy-2, proxy-3)
6. Sobe HAProxy (load balancer L4 TCP)
7. Sobe Prometheus e Grafana

**Tempo estimado:** 60–90 segundos (SQL Servers demoram ~30s para inicializar).

### Portas expostas

| Serviço | Porta host | Uso |
|---|---|---|
| HAProxy (TDS) | `localhost:1433` | Porta principal — conecte aqui |
| SQL Server 1 | `localhost:14331` | Acesso direto ao bucket-1 |
| SQL Server 2 | `localhost:14332` | Acesso direto ao bucket-2 |
| SQL Server 3 | `localhost:14333` | Acesso direto ao bucket-3 |
| Redis | `localhost:6379` | Coordenação distribuída |
| Proxy 1 Health | `localhost:18081` | Health check proxy-1 |
| Proxy 2 Health | `localhost:18082` | Health check proxy-2 |
| Proxy 3 Health | `localhost:18083` | Health check proxy-3 |
| Proxy 1 Metrics | `localhost:19091` | Prometheus metrics proxy-1 |
| Proxy 2 Metrics | `localhost:19092` | Prometheus metrics proxy-2 |
| Proxy 3 Metrics | `localhost:19093` | Prometheus metrics proxy-3 |
| HAProxy Stats | `localhost:8404` | Dashboard HAProxy |
| Prometheus | `localhost:9090` | UI Prometheus |
| Grafana | `localhost:3000` | Dashboards (admin/admin) |

---

## 5. Verificar se os containers estão saudáveis

```bash
# Ver status de todos os containers
docker compose ps
```

**O que verificar:** Todos devem estar com status `Up` ou `Up (healthy)`. O container `init-db` deve estar `Exited (0)` (roda uma vez e para).

```bash
# Se algum container não subiu, ver os logs
docker compose logs sqlserver-bucket-1
docker compose logs proxy-1
docker compose logs haproxy
```

```bash
# Aguardar todos os health checks passarem (pode levar ~60s)
# Verificar SQL Servers:
docker inspect --format='{{.State.Health.Status}}' sqlserver-bucket-1
docker inspect --format='{{.State.Health.Status}}' sqlserver-bucket-2
docker inspect --format='{{.State.Health.Status}}' sqlserver-bucket-3
```

**Saída esperada para cada um:** `healthy`

---

## 6. Smoke tests — conectividade básica

### 6.1 Conectar via HAProxy → Proxy → SQL Server (bucket-1)

```bash
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword1' -d tenant_db -Q "SELECT 1 AS resultado" -C
```

**O que faz:** Conecta pela porta 1433 do HAProxy, que roteia para um dos 3 proxies, que por sua vez conecta no SQL Server do bucket-1. O `-C` aceita certificados auto-assinados.

**Saída esperada:**
```
resultado
-----------
          1
```

### 6.2 Testar cada bucket individualmente

```bash
# Bucket 1
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword1' -d tenant_db -Q "SELECT 'bucket-1' AS bucket, @@SERVERNAME AS server" -C

# Bucket 2
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword2' -d tenant_db -Q "SELECT 'bucket-2' AS bucket, @@SERVERNAME AS server" -C

# Bucket 3
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword3' -d tenant_db -Q "SELECT 'bucket-3' AS bucket, @@SERVERNAME AS server" -C
```

### 6.3 Testar acesso direto ao SQL Server (bypass do proxy)

```bash
# Direto no SQL Server 1 (sem proxy)
sqlcmd -S localhost,14331 -U sa -P 'YourStr0ngP@ssword1' -d tenant_db -Q "SELECT 1" -C

# Direto no SQL Server 2
sqlcmd -S localhost,14332 -U sa -P 'YourStr0ngP@ssword2' -d tenant_db -Q "SELECT 1" -C

# Direto no SQL Server 3
sqlcmd -S localhost,14333 -U sa -P 'YourStr0ngP@ssword3' -d tenant_db -Q "SELECT 1" -C
```

---

## 7. Testes de operações de dados

```bash
# INSERT
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword1' -d tenant_db -Q "
INSERT INTO test_table (name, value) VALUES ('test-exec', 42);
SELECT SCOPE_IDENTITY() AS inserted_id;
" -C

# SELECT (verificar que o dado foi inserido)
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword1' -d tenant_db -Q "
SELECT TOP 5 * FROM test_table ORDER BY id DESC;
" -C

# UPDATE
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword1' -d tenant_db -Q "
UPDATE test_table SET value = 99 WHERE name = 'test-exec';
SELECT * FROM test_table WHERE name = 'test-exec';
" -C

# DELETE
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword1' -d tenant_db -Q "
DELETE FROM test_table WHERE name = 'test-exec';
" -C
```

**O que testa:** CRUD completo passando pelo proxy de forma transparente.

---

## 8. Testes de transação

```bash
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword1' -d tenant_db -Q "
BEGIN TRANSACTION;
INSERT INTO test_table (name, value) VALUES ('tx-test', 100);
INSERT INTO test_table (name, value) VALUES ('tx-test', 200);
COMMIT TRANSACTION;
SELECT * FROM test_table WHERE name = 'tx-test';
" -C
```

**O que testa:** Transações explícitas passando pelo proxy. O proxy faz relay transparente do `BEGIN TRAN` / `COMMIT`.

```bash
# Limpar dados de teste
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword1' -d tenant_db -Q "
DELETE FROM test_table WHERE name = 'tx-test';
" -C
```

---

## 9. Verificar health checks

### 9.1 Liveness (o proxy está vivo?)

```bash
# Proxy 1
curl -s http://localhost:18081/health/live | jq .

# Proxy 2
curl -s http://localhost:18082/health/live | jq .

# Proxy 3
curl -s http://localhost:18083/health/live | jq .
```

**Saída esperada:**
```json
{
  "status": "alive",
  "time": "2025-01-15T10:30:00Z"
}
```

### 9.2 Readiness (proxy + Redis + SQL Servers estão OK?)

```bash
# Relatório completo com latência de cada componente
curl -s http://localhost:18081/health | jq .
```

**Saída esperada:**
```json
{
  "status": "healthy",
  "timestamp": "2025-01-15T10:30:00Z",
  "instance_id": "proxy-1",
  "components": [
    { "name": "redis", "status": "healthy", "latency": "1.2ms" },
    { "name": "sqlserver-bucket-001", "status": "healthy", "latency": "5.3ms" },
    { "name": "sqlserver-bucket-002", "status": "healthy", "latency": "4.8ms" },
    { "name": "sqlserver-bucket-003", "status": "healthy", "latency": "3.9ms" }
  ]
}
```

### 9.3 Verificar todos de uma vez

```bash
# Health check resumido dos 3 proxies
for port in 18081 18082 18083; do
  echo "=== Proxy (porta $port) ==="
  curl -s http://localhost:$port/health | jq '.status, .instance_id'
  echo ""
done
```

---

## 10. Verificar métricas Prometheus

### 10.1 Métricas brutas de um proxy

```bash
# Ver todas as métricas do proxy-1
curl -s http://localhost:19091/metrics | grep "^proxy_"
```

**O que mostra:** Todas as métricas com prefixo `proxy_` — conexões ativas, idle, fila, erros, etc.

### 10.2 Métricas específicas

```bash
# Conexões ativas por bucket
curl -s http://localhost:19091/metrics | grep proxy_connections_active

# Profundidade da fila por bucket
curl -s http://localhost:19091/metrics | grep proxy_queue_length

# Erros de conexão
curl -s http://localhost:19091/metrics | grep proxy_connection_errors_total

# Operações Redis
curl -s http://localhost:19091/metrics | grep proxy_redis_operations_total

# Heartbeat
curl -s http://localhost:19091/metrics | grep proxy_instance_heartbeat
```

### 10.3 Métricas de todos os proxies

```bash
for port in 19091 19092 19093; do
  echo "=== Metrics (porta $port) ==="
  curl -s http://localhost:$port/metrics | grep proxy_connections_active
  echo ""
done
```

---

## 11. Verificar Redis (coordenação distribuída)

### 11.1 Verificar contadores de conexão

```bash
# Contadores globais de cada bucket
redis-cli -p 6379 GET proxy:bucket:bucket-001:count
redis-cli -p 6379 GET proxy:bucket:bucket-001:max
redis-cli -p 6379 GET proxy:bucket:bucket-002:count
redis-cli -p 6379 GET proxy:bucket:bucket-002:max
redis-cli -p 6379 GET proxy:bucket:bucket-003:count
redis-cli -p 6379 GET proxy:bucket:bucket-003:max
```

### 11.2 Verificar instâncias registradas

```bash
# Quais instâncias do proxy estão ativas
redis-cli -p 6379 SMEMBERS proxy:instances

# Conexões por instância (hash de bucket→count)
redis-cli -p 6379 HGETALL proxy:instance:proxy-1:conns
redis-cli -p 6379 HGETALL proxy:instance:proxy-2:conns
redis-cli -p 6379 HGETALL proxy:instance:proxy-3:conns
```

### 11.3 Verificar heartbeats

```bash
# Heartbeat de cada instância (com TTL)
redis-cli -p 6379 TTL proxy:instance:proxy-1:heartbeat
redis-cli -p 6379 TTL proxy:instance:proxy-2:heartbeat
redis-cli -p 6379 TTL proxy:instance:proxy-3:heartbeat
```

**Saída esperada:** Valores entre 1 e 30 (segundos restantes do TTL). Se retornar `-2`, o heartbeat expirou (instância morta).

### 11.4 Ver todas as chaves do proxy no Redis

```bash
redis-cli -p 6379 KEYS "proxy:*"
```

### 11.5 Alternativa: usar redis-cli via Docker

```bash
# Se não tem redis-cli instalado localmente
docker exec redis redis-cli SMEMBERS proxy:instances
docker exec redis redis-cli GET proxy:bucket:bucket-001:count
docker exec redis redis-cli KEYS "proxy:*"
```

---

## 12. Testes de carga — conexões concorrentes

### 12.1 Teste básico de conexões concorrentes + fila

```bash
# Voltar para a raiz do projeto
cd ..

# Executar teste de conexões concorrentes e fila de espera
go run scripts/test_phase4.go
```

**O que faz:**
1. **Teste 1:** Abre 10 conexões simultâneas via HAProxy, cada uma executa `SELECT @id` e fecha. Valida que o proxy funciona com concorrência.
2. **Teste 2:** Abre 20 conexões "holders" que ficam segurando por 10 segundos, depois tenta abrir 5 conexões extras que devem ser processadas via fila ou diretamente quando slots são liberados.

**Saída esperada:** `✅ Teste de conexões concorrentes OK` e `✅ Teste de saturação e fila OK`.

**Tempo de execução:** ~15 segundos.

---

## 13. Teste de Queue Timeout (erro 50004)

```bash
go run scripts/test_phase4_timeout.go
```

**O que faz:**
1. Reduz `max_connections` do bucket-001 para 3 (via Redis)
2. Abre 3 conexões holders que seguram por 40 segundos (maior que o `queue_timeout` de 30s)
3. Tenta abrir uma conexão extra — deve entrar na fila e receber TDS Error 50004 após ~30s
4. Restaura a configuração original no final

**Saída esperada:** `✅ TDS Error 50004 (Queue Timeout) recebido corretamente!`

**Tempo de execução:** ~35 segundos (espera o queue_timeout de 30s).

---

## 14. Teste de Circuit Breaker (erro 50005)

```bash
go run scripts/test_phase4_circuit_breaker.go
```

**O que faz:**
1. Reduz `max_connections` do bucket-001 para 3 (via Redis)
2. Abre 3 conexões holders (satura o bucket)
3. Envia 2 conexões para preencher a fila (max_queue_size=2)
4. Envia uma 6ª conexão — deve ser rejeitada **instantaneamente** pelo circuit breaker com TDS Error 50005
5. Restaura tudo no final

**Saída esperada:** `✅ TDS Error 50005 (Queue Full / Circuit Breaker) recebido!` com rejeição rápida (< 5s).

**Tempo de execução:** ~15 segundos.

---

## 15. Teste de Circuit Breaker via proxy direto

```bash
go run scripts/test_phase4_cb_direct.go
```

**O que faz:** Similar ao teste anterior, mas envia 10 conexões extras em paralelo via HAProxy para forçar que pelo menos um proxy atinja a profundidade máxima da fila. Conta quantas foram rejeitadas por circuit breaker (50005), timeout (50004) e quantas tiveram sucesso.

**Saída esperada:** Pelo menos algumas conexões com `CIRCUIT BREAKER (50005)`.

**Tempo de execução:** ~15–30 segundos.

---

## 16. Teste de cenários de erro combinados

```bash
go run scripts/test_phase4_errors.go
```

**O que faz:** Executa teste de Queue Timeout com redução de max_connections para 3, satura o bucket, e verifica que a conexão extra recebe erro 50004 (timeout) ou 50005 (queue full). Restaura tudo no final.

**Saída esperada:** `✅ Teste de Queue Timeout concluído`.

**Tempo de execução:** ~15 segundos.

---

## 17. Verificar logs do proxy

### 17.1 Logs em tempo real de todos os proxies

```bash
cd deployments
docker compose logs -f proxy-1 proxy-2 proxy-3
```

### 17.2 Logs de um proxy específico

```bash
docker compose logs -f proxy-1
```

### 17.3 Filtrar logs por padrões importantes

```bash
# Ver sessões sendo aceitas/fechadas
docker compose logs proxy-1 2>&1 | grep -i "session"

# Ver operações de acquire/release de slots
docker compose logs proxy-1 2>&1 | grep -i "acquire\|release"

# Ver erros
docker compose logs proxy-1 2>&1 | grep -i "error\|fail\|warn"

# Ver heartbeat
docker compose logs proxy-1 2>&1 | grep -i "heartbeat"

# Ver fallback mode
docker compose logs proxy-1 2>&1 | grep -i "fallback"

# Ver fila de espera
docker compose logs proxy-1 2>&1 | grep -i "queue\|circuit"
```

### 17.4 Logs dos SQL Servers

```bash
docker compose logs sqlserver-bucket-1 2>&1 | tail -20
```

### 17.5 Logs do HAProxy

```bash
docker compose logs haproxy 2>&1 | tail -20
```

---

## 18. Monitoramento — Grafana e HAProxy Stats

### 18.1 Grafana

Abrir no navegador:

```
http://localhost:3000
```

**Credenciais:** `admin` / `admin`

O Grafana já vem pré-configurado com Prometheus como datasource. As métricas dos proxies são coletadas automaticamente.

**Queries úteis no Explore:**
- `proxy_connections_active` — conexões ativas por bucket
- `rate(proxy_connections_total[5m])` — taxa de conexões por segundo
- `proxy_queue_length` — profundidade da fila
- `histogram_quantile(0.95, rate(proxy_queue_wait_duration_seconds_bucket[5m]))` — P95 de tempo na fila

### 18.2 Prometheus

```
http://localhost:9090
```

Na aba "Status > Targets", verificar que todos os 3 proxies estão como `UP`.

### 18.3 HAProxy Stats

```
http://localhost:8404/stats
```

**O que mostra:** Conexões ativas, bytes transferidos, health check status de cada proxy.

---

## 19. Testes de resiliência

### 19.1 Matar um proxy e verificar failover

```bash
cd deployments

# Matar proxy-2
docker compose stop proxy-2

# Verificar que proxy-1 e proxy-3 continuam respondendo
curl -s http://localhost:18081/health/live | jq .
curl -s http://localhost:18083/health/live | jq .

# Testar conexão via HAProxy (deve rotear para proxy-1 ou proxy-3)
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword1' -d tenant_db -Q "SELECT 1" -C

# Verificar que o HAProxy stats mostra proxy-2 como DOWN
# Abrir: http://localhost:8404/stats

# Verificar que o Redis detectou proxy-2 como morto (após ~30s)
sleep 35
redis-cli -p 6379 SMEMBERS proxy:instances
# Deve mostrar apenas proxy-1 e proxy-3

# Subir proxy-2 novamente
docker compose start proxy-2
sleep 10

# Verificar que proxy-2 voltou
curl -s http://localhost:18082/health/live | jq .
redis-cli -p 6379 SMEMBERS proxy:instances
```

### 19.2 Matar Redis e verificar fallback mode

```bash
# Matar Redis
docker compose stop redis

# Os proxies devem entrar em fallback mode
# Verificar nos logs:
docker compose logs --since 10s proxy-1 2>&1 | grep -i fallback

# Conexões ainda devem funcionar (com limite local reduzido: max/3)
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword1' -d tenant_db -Q "SELECT 'fallback mode works' AS result" -C

# Subir Redis novamente
docker compose start redis
sleep 10

# Os proxies devem sair do fallback e reconciliar contadores
docker compose logs --since 20s proxy-1 2>&1 | grep -i "fallback\|reconcil"
```

### 19.3 Matar um SQL Server

```bash
# Matar SQL Server do bucket-1
docker compose stop sqlserver-bucket-1

# Verificar health — bucket-1 deve aparecer como unhealthy
curl -s http://localhost:18081/health | jq '.components[] | select(.name | contains("bucket-001"))'

# Tentar conectar ao bucket-1 (deve falhar com erro do proxy)
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword1' -d tenant_db -Q "SELECT 1" -C
# Esperado: erro de conexão

# Os outros buckets devem continuar funcionando
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword2' -d tenant_db -Q "SELECT 'bucket-2 ok' AS result" -C

# Restaurar
docker compose start sqlserver-bucket-1
sleep 30  # Esperar health check do SQL Server
```

### 19.4 Restart rolling dos proxies

```bash
# Reiniciar proxies um a um (sem downtime)
docker compose restart proxy-1
sleep 15
docker compose restart proxy-2
sleep 15
docker compose restart proxy-3
sleep 15

# Verificar que todos voltaram
for port in 18081 18082 18083; do
  echo "Porta $port:"
  curl -s http://localhost:$port/health/live | jq .status
done
```

---

## 20. Parar e limpar tudo

### 20.1 Parar containers (preserva dados)

```bash
cd deployments
docker compose down
```

**O que faz:** Para todos os containers mas mantém os volumes (dados dos SQL Servers, Redis, Prometheus, Grafana).

### 20.2 Parar e destruir tudo (incluindo dados)

```bash
docker compose down -v
```

**O que faz:** Para todos os containers E remove todos os volumes. Na próxima subida, os SQL Servers serão re-inicializados do zero.

### 20.3 Limpar imagens Docker do build

```bash
docker compose down -v --rmi local
```

**O que faz:** Remove tudo incluindo as imagens Docker compiladas. Força rebuild completo no próximo `up --build`.

### 20.4 Limpar artefatos locais Go

```bash
cd ..
rm -rf bin/ coverage.out coverage.html
```

---

## 21. Comandos úteis do Makefile

O projeto inclui um `Makefile` com atalhos. Use `make help` para ver todos:

```bash
make help
```

### Tabela de atalhos

| Comando | Equivale a | Descrição |
|---|---|---|
| `make build` | `go build -o bin/proxy ./cmd/proxy/` | Compila o binário do proxy |
| `make run` | Build + executa localmente | Roda o proxy fora do Docker |
| `make test` | `go test -v -race ./...` | Roda testes unitários com race detector |
| `make test-coverage` | Testes + relatório HTML | Gera `coverage.html` |
| `make lint` | `golangci-lint run ./...` | Linter (precisa instalar golangci-lint) |
| `make fmt` | `gofmt -s -w .` | Formata o código Go |
| `make vet` | `go vet ./...` | Análise estática |
| `make deps` | `go mod download && go mod tidy` | Baixa dependências |
| `make clean` | `rm -rf bin/ coverage.*` | Limpa artefatos |
| `make docker-build` | `docker compose build` | Build das imagens Docker |
| `make docker-up` | `docker compose up -d` | Sobe todos os containers |
| `make docker-down` | `docker compose down` | Para todos os containers |
| `make docker-destroy` | `docker compose down -v` | Para + remove volumes |
| `make docker-restart` | `docker compose restart` | Reinicia todos |
| `make docker-logs` | `docker compose logs -f` | Logs em tempo real (todos) |
| `make docker-logs-proxy` | `docker compose logs -f proxy-*` | Logs só dos proxies |
| `make docker-logs-sql` | `docker compose logs -f sqlserver-*` | Logs só dos SQL Servers |
| `make docker-ps` | `docker compose ps` | Status dos containers |
| `make health` | curl nos 3 proxies | Health check formatado |
| `make redis-ping` | `redis-cli ping` | Verifica se Redis está vivo |
| `make redis-info` | `redis-cli info` | Info do Redis |

---

## Resumo — Execução rápida (copiar e colar)

Se quiser executar tudo de uma vez, aqui está o fluxo mínimo:

```bash
# 1. Build + sobe tudo
cd deployments && docker compose up -d --build

# 2. Esperar ~60s para tudo ficar healthy
sleep 60

# 3. Verificar containers
docker compose ps

# 4. Smoke test
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword1' -d tenant_db -Q "SELECT 1" -C

# 5. Health check
curl -s http://localhost:18081/health | jq .

# 6. Testar os 3 buckets
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword1' -d tenant_db -Q "SELECT 'bucket-1 OK'" -C
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword2' -d tenant_db -Q "SELECT 'bucket-2 OK'" -C
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword3' -d tenant_db -Q "SELECT 'bucket-3 OK'" -C

# 7. Transação
sqlcmd -S localhost,1433 -U sa -P 'YourStr0ngP@ssword1' -d tenant_db -Q "BEGIN TRAN; SELECT 1; COMMIT;" -C

# 8. Redis
redis-cli -p 6379 SMEMBERS proxy:instances
redis-cli -p 6379 KEYS "proxy:*"

# 9. Testes de carga
cd .. && go run scripts/test_phase4.go

# 10. Métricas
curl -s http://localhost:19091/metrics | grep proxy_connections_active
```
