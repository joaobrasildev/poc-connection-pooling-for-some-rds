# ============================================================================
# Makefile — Connection Pooling Proxy for SQL Server
# ============================================================================

.PHONY: help build run test clean docker-up docker-down docker-logs docker-ps \
        docker-build health lint fmt vet deps docker-restart docker-infra-up \
        docker-infra-down

# Default target
help: ## Show this help
	@echo "╔══════════════════════════════════════════════════════════╗"
	@echo "║    Connection Pooling Proxy — Make Targets              ║"
	@echo "╚══════════════════════════════════════════════════════════╝"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# ── Go Build ─────────────────────────────────────────────────────────────

build: ## Build the proxy binary
	@echo "🔨 Building proxy..."
	go build -o bin/proxy ./cmd/proxy/
	@echo "✅ Binary: bin/proxy"

build-loadgen: ## Build the load generator binary
	@echo "🔨 Building load generator..."
	go build -o bin/loadgen ./cmd/loadgen/
	@echo "✅ Binary: bin/loadgen"

build-all: build build-loadgen ## Build all binaries

run: build ## Build and run the proxy locally
	@echo "🚀 Running proxy..."
	./bin/proxy --config configs/proxy.yaml --buckets configs/buckets.yaml

test: ## Run all tests
	@echo "🧪 Running tests..."
	go test -v -race -coverprofile=coverage.out ./...

test-coverage: test ## Run tests with coverage report
	go tool cover -html=coverage.out -o coverage.html
	@echo "✅ Coverage report: coverage.html"

lint: ## Run linter (requires golangci-lint)
	@echo "🔍 Linting..."
	golangci-lint run ./... || echo "⚠️  Install golangci-lint: brew install golangci-lint"

fmt: ## Format Go source code
	@echo "📝 Formatting..."
	gofmt -s -w .

vet: ## Run go vet
	@echo "🔎 Vetting..."
	go vet ./...

deps: ## Download and tidy dependencies
	@echo "📦 Downloading dependencies..."
	go mod download
	go mod tidy
	@echo "✅ Dependencies ready"

clean: ## Clean build artifacts
	@echo "🧹 Cleaning..."
	rm -rf bin/ coverage.out coverage.html
	@echo "✅ Clean"

# ── Docker ───────────────────────────────────────────────────────────────

COMPOSE_FILE := deployments/docker-compose.yml
COMPOSE := docker compose -f $(COMPOSE_FILE) -p poc-proxy

docker-build: ## Build Docker images
	@echo "🐳 Building Docker images..."
	$(COMPOSE) build

docker-up: ## Start all containers (SQL Server, Redis, Proxy, HAProxy, Prometheus, Grafana)
	@echo "🐳 Starting all services..."
	$(COMPOSE) up -d
	@echo ""
	@echo "╔══════════════════════════════════════════════════════════╗"
	@echo "║  Services Starting...                                   ║"
	@echo "╠══════════════════════════════════════════════════════════╣"
	@echo "║  SQL Server 1:    localhost:14331                       ║"
	@echo "║  SQL Server 2:    localhost:14332                       ║"
	@echo "║  SQL Server 3:    localhost:14333                       ║"
	@echo "║  Redis:           localhost:6379                        ║"
	@echo "║  Proxy 1 Health:  http://localhost:18081/health         ║"
	@echo "║  Proxy 2 Health:  http://localhost:18082/health         ║"
	@echo "║  Proxy 3 Health:  http://localhost:18083/health         ║"
	@echo "║  HAProxy Stats:   http://localhost:8404/stats           ║"
	@echo "║  Prometheus:      http://localhost:9090                 ║"
	@echo "║  Grafana:         http://localhost:3000 (admin/admin)   ║"
	@echo "╚══════════════════════════════════════════════════════════╝"
	@echo ""
	@echo "⏳ SQL Server containers take ~30s to become healthy."
	@echo "   Run 'make docker-logs' to monitor startup progress."

docker-infra-up: ## Start only infrastructure (SQL Server, Redis) without proxy
	@echo "🐳 Starting infrastructure only..."
	$(COMPOSE) up -d sqlserver-bucket-1 sqlserver-bucket-2 sqlserver-bucket-3 redis
	@echo "⏳ SQL Server containers take ~30s to become healthy."

docker-down: ## Stop all containers
	@echo "🐳 Stopping all services..."
	$(COMPOSE) down

docker-destroy: ## Stop all containers and remove volumes
	@echo "🐳 Destroying all services and data..."
	$(COMPOSE) down -v

docker-restart: ## Restart all containers
	@echo "🐳 Restarting all services..."
	$(COMPOSE) restart

docker-logs: ## Follow logs from all containers
	$(COMPOSE) logs -f

docker-logs-proxy: ## Follow logs from proxy containers only
	$(COMPOSE) logs -f proxy-1 proxy-2 proxy-3

docker-logs-sql: ## Follow logs from SQL Server containers only
	$(COMPOSE) logs -f sqlserver-bucket-1 sqlserver-bucket-2 sqlserver-bucket-3

docker-ps: ## Show container status
	$(COMPOSE) ps

# ── Health & Diagnostics ────────────────────────────────────────────────

health: ## Check health of all proxy instances
	@echo "🏥 Checking proxy health..."
	@echo ""
	@echo "Proxy 1:"
	@curl -s http://localhost:18081/health | python3 -m json.tool 2>/dev/null || echo "  ❌ Not reachable"
	@echo ""
	@echo "Proxy 2:"
	@curl -s http://localhost:18082/health | python3 -m json.tool 2>/dev/null || echo "  ❌ Not reachable"
	@echo ""
	@echo "Proxy 3:"
	@curl -s http://localhost:18083/health | python3 -m json.tool 2>/dev/null || echo "  ❌ Not reachable"

redis-ping: ## Ping Redis
	@echo "🔴 Pinging Redis..."
	@redis-cli -p 6379 ping 2>/dev/null || docker exec redis redis-cli ping

redis-info: ## Show Redis info
	@docker exec redis redis-cli info server | head -20
	@echo "..."
	@docker exec redis redis-cli info memory | head -10
