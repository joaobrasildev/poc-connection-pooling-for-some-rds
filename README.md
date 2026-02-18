# Connection Pooling Proxy for SQL Server — POC

## Visão Geral

Proxy de connection pooling em Go que se posiciona entre coreVMs (Harbour) e instâncias RDS SQL Server, controlando o número máximo de conexões simultâneas por bucket via protocolo TDS transparente.

## Pré-requisitos

- **Go** 1.22+
- **Docker** + Docker Compose
- **Make**

## Quick Start

```bash
# 1. Instalar dependências Go
make deps

# 2. Build do projeto
make build

# 3. Subir toda a infraestrutura (SQL Server, Redis, Proxy, HAProxy, Prometheus, Grafana)
make docker-up

# 4. Verificar saúde dos serviços
make health

# 5. Acompanhar logs
make docker-logs
```

## Endpoints Disponíveis

| Serviço | URL | Credenciais |
|---|---|---|
| SQL Server Bucket 1 | `localhost:14331` | sa / YourStr0ngP@ssword1 |
| SQL Server Bucket 2 | `localhost:14332` | sa / YourStr0ngP@ssword2 |
| SQL Server Bucket 3 | `localhost:14333` | sa / YourStr0ngP@ssword3 |
| Redis | `localhost:6379` | — |
| Proxy 1 Health | http://localhost:18081/health | — |
| Proxy 2 Health | http://localhost:18082/health | — |
| Proxy 3 Health | http://localhost:18083/health | — |
| Proxy 1 Metrics | http://localhost:19091/metrics | — |
| HAProxy Stats | http://localhost:8404/stats | — |
| Prometheus | http://localhost:9090 | — |
| Grafana | http://localhost:3000 | admin / admin |

## Estrutura do Projeto

```
├── cmd/
│   ├── proxy/          # Entrypoint do proxy
│   └── loadgen/        # Gerador de carga (Phase 6)
├── internal/
│   ├── config/         # Configuração YAML
│   ├── proxy/          # Core do proxy TDS (Phase 2)
│   ├── pool/           # Connection pool manager (Phase 1)
│   ├── coordinator/    # Coordenação Redis (Phase 3)
│   ├── queue/          # Fila de espera (Phase 4)
│   ├── metrics/        # Prometheus metrics
│   ├── health/         # Health checks
│   └── tds/            # TDS protocol parser (Phase 2)
├── pkg/bucket/         # Modelo de bucket
├── configs/            # YAML configs
├── deployments/        # Docker Compose + HAProxy
├── scripts/            # SQL init/seed scripts
├── prometheus/         # Prometheus config
├── grafana/            # Grafana dashboards + provisioning
├── Dockerfile          # Multi-stage build
└── Makefile            # Build system
```

## Make Targets

```bash
make help              # Lista todos os targets
make build             # Build do proxy
make test              # Testes com race detector
make docker-up         # Sobe toda a stack
make docker-down       # Para toda a stack
make docker-destroy    # Para e remove volumes
make docker-logs       # Logs de todos os containers
make health            # Verifica saúde dos proxies
make redis-ping        # Ping no Redis
```

## Fases de Desenvolvimento

- [x] **Fase 0** — Setup do projeto e infraestrutura Docker
- [x] **Fase 1** — Connection Pool Manager
- [x] **Fase 2** — Proxy TDS (Wire Protocol)
- [x] **Fase 3** — Coordenação Distribuída (Redis)
- [x] **Fase 4** — Fila de Espera e Backpressure
- [x] **Fase 5** — Observabilidade
- [x] **Fase 6** — Load Generator e Testes
- [x] **Fase 7** — Hardening e Documentação
