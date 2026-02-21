// Package health fornece funcionalidade de health check para todos os componentes de infraestrutura.
// Verifica conectividade com instâncias SQL Server (buckets) e Redis.
package health

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/joao-brasil/poc-connection-pooling/internal/config"
	"github.com/joao-brasil/poc-connection-pooling/pkg/bucket"
	_ "github.com/microsoft/go-mssqldb"
	"github.com/redis/go-redis/v9"
)

// Status representa o status de saúde de um componente.
type Status string

const (
	StatusHealthy   Status = "healthy"
	StatusUnhealthy Status = "unhealthy"
)

// ComponentHealth representa a saúde de um único componente.
type ComponentHealth struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Message string `json:"message,omitempty"`
	Latency string `json:"latency"`
}

// HealthReport é o relatório geral de saúde.
type HealthReport struct {
	Status     Status            `json:"status"`
	Timestamp  string            `json:"timestamp"`
	InstanceID string            `json:"instance_id"`
	Components []ComponentHealth `json:"components"`
}

// Checker realiza health checks contra componentes de infraestrutura.
type Checker struct {
	cfg         *config.Config
	redisClient *redis.Client
}

// NewChecker cria um novo health checker.
func NewChecker(cfg *config.Config) *Checker {
	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.Redis.Addr,
		Password:     cfg.Redis.Password,
		DB:           cfg.Redis.DB,
		DialTimeout:  cfg.Redis.DialTimeout,
		ReadTimeout:  cfg.Redis.ReadTimeout,
		WriteTimeout: cfg.Redis.WriteTimeout,
	})

	return &Checker{
		cfg:         cfg,
		redisClient: rdb,
	}
}

// Close limpa os recursos.
func (c *Checker) Close() error {
	return c.redisClient.Close()
}

// Check realiza health checks em todos os componentes e retorna um relatório.
func (c *Checker) Check(ctx context.Context) *HealthReport {
	report := &HealthReport{
		Status:     StatusHealthy,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		InstanceID: c.cfg.Proxy.InstanceID,
	}

	var (
		mu         sync.Mutex
		wg         sync.WaitGroup
		components []ComponentHealth
	)

	// Verificar Redis
	wg.Add(1)
	go func() {
		defer wg.Done()
		ch := c.checkRedis(ctx)
		mu.Lock()
		components = append(components, ch)
		mu.Unlock()
	}()

	// Verificar cada bucket SQL Server
	for i := range c.cfg.Buckets {
		b := &c.cfg.Buckets[i]
		wg.Add(1)
		go func(bkt *bucket.Bucket) {
			defer wg.Done()
			ch := c.checkSQLServer(ctx, bkt)
			mu.Lock()
			components = append(components, ch)
			mu.Unlock()
		}(b)
	}

	wg.Wait()

	report.Components = components

	// Se qualquer componente estiver unhealthy, marcar geral como unhealthy
	for _, comp := range components {
		if comp.Status == StatusUnhealthy {
			report.Status = StatusUnhealthy
			break
		}
	}

	return report
}

// checkRedis verifica a conectividade com o Redis.
func (c *Checker) checkRedis(ctx context.Context) ComponentHealth {
	start := time.Now()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result := c.redisClient.Ping(ctx)
	latency := time.Since(start)

	if result.Err() != nil {
		return ComponentHealth{
			Name:    "redis",
			Status:  StatusUnhealthy,
			Message: fmt.Sprintf("PING failed: %v", result.Err()),
			Latency: latency.String(),
		}
	}

	return ComponentHealth{
		Name:    "redis",
		Status:  StatusHealthy,
		Message: "PONG",
		Latency: latency.String(),
	}
}

// checkSQLServer verifica a conectividade com uma instância SQL Server.
func (c *Checker) checkSQLServer(ctx context.Context, b *bucket.Bucket) ComponentHealth {
	start := time.Now()
	name := fmt.Sprintf("sqlserver-%s", b.ID)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	db, err := sql.Open("sqlserver", b.DSN())
	if err != nil {
		return ComponentHealth{
			Name:    name,
			Status:  StatusUnhealthy,
			Message: fmt.Sprintf("failed to create connection: %v", err),
			Latency: time.Since(start).String(),
		}
	}
	defer db.Close()

	// Testar conectividade com SELECT 1
	var result int
	err = db.QueryRowContext(ctx, "SELECT 1").Scan(&result)
	latency := time.Since(start)

	if err != nil {
		return ComponentHealth{
			Name:    name,
			Status:  StatusUnhealthy,
			Message: fmt.Sprintf("SELECT 1 failed: %v", err),
			Latency: latency.String(),
		}
	}

	// Também verificar versão do servidor
	var version string
	err = db.QueryRowContext(ctx, "SELECT @@VERSION").Scan(&version)
	if err != nil {
		return ComponentHealth{
			Name:    name,
			Status:  StatusHealthy,
			Message: "connected (version check failed)",
			Latency: latency.String(),
		}
	}

	// Truncar string de versão para legibilidade
	if len(version) > 80 {
		version = version[:80] + "..."
	}

	return ComponentHealth{
		Name:    name,
		Status:  StatusHealthy,
		Message: version,
		Latency: latency.String(),
	}
}

// ServeHTTP inicia o servidor HTTP de health check.
func (c *Checker) ServeHTTP(ctx context.Context) *http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		report := c.Check(r.Context())

		w.Header().Set("Content-Type", "application/json")
		if report.Status == StatusUnhealthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}

		json.NewEncoder(w).Encode(report)
	})

	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		report := c.Check(r.Context())

		w.Header().Set("Content-Type", "application/json")
		if report.Status == StatusUnhealthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}

		json.NewEncoder(w).Encode(report)
	})

	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status": "alive",
			"time":   time.Now().UTC().Format(time.RFC3339),
		})
	})

	addr := fmt.Sprintf(":%d", c.cfg.Proxy.HealthCheckPort)
	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	go func() {
		log.Printf("[health] HTTP server listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[health] HTTP server error: %v", err)
		}
	}()

	return server
}
