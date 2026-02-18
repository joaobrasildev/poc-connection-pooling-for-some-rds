// Package metrics defines Prometheus metrics for the proxy.
// This is a placeholder that registers all metric collectors upfront
// so that future phases can use them without modifying this file.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// ConnectionsActive tracks the number of active connections per bucket.
	ConnectionsActive = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "proxy_connections_active",
		Help: "Number of active connections per bucket",
	}, []string{"bucket_id"})

	// ConnectionsIdle tracks the number of idle connections per bucket.
	ConnectionsIdle = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "proxy_connections_idle",
		Help: "Number of idle connections in the pool per bucket",
	}, []string{"bucket_id"})

	// ConnectionsPinned tracks the number of pinned connections per bucket.
	ConnectionsPinned = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "proxy_connections_pinned",
		Help: "Number of pinned connections per bucket",
	}, []string{"bucket_id", "pin_reason"})

	// ConnectionsMax tracks the configured max connections per bucket.
	ConnectionsMax = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "proxy_connections_max",
		Help: "Configured maximum connections per bucket",
	}, []string{"bucket_id"})

	// ConnectionsTotal counts total connection acquire/release operations.
	ConnectionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "proxy_connections_total",
		Help: "Total connection operations",
	}, []string{"bucket_id", "status"})

	// QueueLength tracks the current queue length per bucket.
	QueueLength = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "proxy_queue_length",
		Help: "Number of requests waiting in queue per bucket",
	}, []string{"bucket_id"})

	// QueueWaitDuration tracks the time requests spend waiting in queue.
	QueueWaitDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "proxy_queue_wait_seconds",
		Help:    "Time spent waiting in queue for a connection",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30},
	}, []string{"bucket_id"})

	// TDSPacketsTotal counts TDS packets by direction and type.
	TDSPacketsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "proxy_tds_packets_total",
		Help: "Total TDS packets processed",
	}, []string{"bucket_id", "direction", "type"})

	// QueryDuration tracks query execution time.
	QueryDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "proxy_query_duration_seconds",
		Help:    "Query execution duration",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"bucket_id"})

	// ConnectionErrors counts connection errors by type.
	ConnectionErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "proxy_connection_errors_total",
		Help: "Total connection errors",
	}, []string{"bucket_id", "error_type"})

	// RedisOperations counts Redis operations.
	RedisOperations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "proxy_redis_operations_total",
		Help: "Total Redis operations",
	}, []string{"operation", "status"})

	// InstanceHeartbeat tracks instance heartbeat status.
	InstanceHeartbeat = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "proxy_instance_heartbeat",
		Help: "Instance heartbeat (1 = alive, 0 = dead)",
	}, []string{"instance_id"})

	// PinningDuration tracks how long connections stay pinned.
	PinningDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "proxy_pinning_duration_seconds",
		Help:    "Duration of connection pinning",
		Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60, 300},
	}, []string{"bucket_id", "pin_reason"})
)
