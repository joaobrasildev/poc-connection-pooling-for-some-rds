// Package main is the entrypoint for the connection pooling proxy.
// It loads configuration, initializes health checks and metrics,
// and sets up graceful shutdown handling.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joao-brasil/poc-connection-pooling/internal/config"
	"github.com/joao-brasil/poc-connection-pooling/internal/coordinator"
	"github.com/joao-brasil/poc-connection-pooling/internal/health"
	"github.com/joao-brasil/poc-connection-pooling/internal/metrics"
	"github.com/joao-brasil/poc-connection-pooling/internal/pool"
	"github.com/joao-brasil/poc-connection-pooling/internal/proxy"
	"github.com/joao-brasil/poc-connection-pooling/internal/queue"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	proxyConfigPath   = flag.String("config", "configs/proxy.yaml", "Path to proxy configuration file")
	bucketsConfigPath = flag.String("buckets", "configs/buckets.yaml", "Path to buckets configuration file")
)

func main() {
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("[main] Starting Connection Pooling Proxy for SQL Server")

	// ─── Load Configuration ───────────────────────────────────────────
	cfg, err := config.Load(*proxyConfigPath, *bucketsConfigPath)
	if err != nil {
		log.Fatalf("[main] Failed to load configuration: %v", err)
	}
	log.Printf("[main] Configuration loaded: %d buckets, instance=%s", len(cfg.Buckets), cfg.Proxy.InstanceID)

	for _, b := range cfg.Buckets {
		log.Printf("[main]   Bucket %s → %s:%d (max_conn=%d, min_idle=%d)",
			b.ID, b.Host, b.Port, b.MaxConnections, b.MinIdle)
	}

	// ─── Initialize Metrics ──────────────────────────────────────────
	// Pre-register metric labels for each bucket so Grafana shows them immediately
	for _, b := range cfg.Buckets {
		metrics.ConnectionsActive.WithLabelValues(b.ID).Set(0)
		metrics.ConnectionsIdle.WithLabelValues(b.ID).Set(0)
		metrics.ConnectionsMax.WithLabelValues(b.ID).Set(float64(b.MaxConnections))
		metrics.QueueLength.WithLabelValues(b.ID).Set(0)
	}
	metrics.InstanceHeartbeat.WithLabelValues(cfg.Proxy.InstanceID).Set(1)

	// Metrics HTTP server (Prometheus scrape endpoint)
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsServer := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Proxy.MetricsPort),
		Handler:      metricsMux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("[main] Metrics server listening on :%d/metrics", cfg.Proxy.MetricsPort)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[main] Metrics server error: %v", err)
		}
	}()

	// ─── Initialize Health Checker ───────────────────────────────────
	checker := health.NewChecker(cfg)
	healthServer := checker.ServeHTTP(context.Background())
	log.Printf("[main] Health check server listening on :%d/health", cfg.Proxy.HealthCheckPort)

	// ─── Run Initial Health Check ────────────────────────────────────
	log.Println("[main] Running initial health check...")
	report := checker.Check(context.Background())
	for _, comp := range report.Components {
		status := "✅"
		if comp.Status == health.StatusUnhealthy {
			status = "❌"
		}
		log.Printf("[main]   %s %s: %s (latency: %s)", status, comp.Name, comp.Message, comp.Latency)
	}
	log.Printf("[main] Overall health: %s", report.Status)

	// ─── Phase 1 — Initialize Connection Pool Manager ─────────
	log.Println("[main] Initializing connection pool manager...")
	poolMgr, err := pool.NewManager(context.Background(), cfg)
	if err != nil {
		log.Fatalf("[main] Failed to initialize pool manager: %v", err)
	}
	defer func() {
		log.Println("[main] Closing pool manager...")
		if err := poolMgr.Close(); err != nil {
			log.Printf("[main] Pool manager close error: %v", err)
		}
	}()
	log.Println("[main] Pool manager ready")
	for _, s := range poolMgr.Stats() {
		log.Printf("[main]   Pool %s: idle=%d, active=%d, max=%d", s.BucketID, s.Idle, s.Active, s.Max)
	}

	// ─── Phase 3 — Initialize Redis Coordinator ─────────────────────
	log.Println("[main] Initializing Redis coordinator...")
	rc, err := coordinator.NewRedisCoordinator(context.Background(), cfg)
	if err != nil {
		log.Fatalf("[main] Failed to initialize Redis coordinator: %v", err)
	}
	defer func() {
		log.Println("[main] Closing Redis coordinator...")
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		if err := rc.Close(shutCtx); err != nil {
			log.Printf("[main] Coordinator close error: %v", err)
		}
	}()
	if rc.IsFallback() {
		log.Println("[main] ⚠️  Coordinator started in FALLBACK mode (Redis unavailable)")
	} else {
		log.Println("[main] Coordinator ready (Redis connected)")
	}

	// Start heartbeat.
	hb := coordinator.NewHeartbeat(rc)
	hb.Start(context.Background())
	defer hb.Stop()

	// ─── Phase 4 — Initialize Distributed Queue ───────────────────────
	dq := queue.NewDistributedQueue(rc, cfg.Proxy.QueueTimeout, cfg.Proxy.MaxQueueSize)
	log.Printf("[main] Distributed queue ready (timeout=%s, max_queue_size=%d)",
		cfg.Proxy.QueueTimeout, cfg.Proxy.MaxQueueSize)

	// ─── Phase 2 — Initialize TDS Proxy ────────────────────────────
	proxyServer := proxy.NewServer(cfg, poolMgr, rc, dq)
	if err := proxyServer.Start(context.Background()); err != nil {
		log.Fatalf("[main] Failed to start TDS proxy: %v", err)
	}
	defer func() {
		log.Println("[main] Stopping TDS proxy...")
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		if err := proxyServer.Stop(shutCtx); err != nil {
			log.Printf("[main] TDS proxy stop error: %v", err)
		}
	}()
	log.Printf("[main] TDS proxy listening on %s:%d", cfg.Proxy.ListenAddr, cfg.Proxy.ListenPort)

	// ─── Graceful Shutdown ───────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	log.Println("[main] Proxy is ready. Waiting for shutdown signal...")
	sig := <-sigCh
	log.Printf("[main] Received signal %v, shutting down gracefully...", sig)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Shutdown in reverse order
	metrics.InstanceHeartbeat.WithLabelValues(cfg.Proxy.InstanceID).Set(0)

	if err := healthServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[main] Health server shutdown error: %v", err)
	}

	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[main] Metrics server shutdown error: %v", err)
	}

	if err := checker.Close(); err != nil {
		log.Printf("[main] Health checker close error: %v", err)
	}

	log.Println("[main] Shutdown complete.")
}
