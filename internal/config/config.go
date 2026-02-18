// Package config handles loading and validating proxy and bucket configuration from YAML files.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/joao-brasil/poc-connection-pooling/pkg/bucket"
	"gopkg.in/yaml.v3"
)

// ProxyConfig holds the main proxy configuration.
type ProxyConfig struct {
	ListenAddr          string        `yaml:"listen_addr"`
	ListenPort          int           `yaml:"listen_port"`
	InstanceID          string        `yaml:"instance_id"`
	SessionTimeout      time.Duration `yaml:"session_timeout"`
	IdleTimeout         time.Duration `yaml:"idle_timeout"`
	QueueTimeout        time.Duration `yaml:"queue_timeout"`
	MaxQueueSize        int           `yaml:"max_queue_size"`
	PinningMode         string        `yaml:"pinning_mode"`
	HealthCheckInterval time.Duration `yaml:"health_check_interval"`
	HealthCheckPort     int           `yaml:"health_check_port"`
	MetricsPort         int           `yaml:"metrics_port"`
}

// RedisConfig holds the Redis connection configuration.
type RedisConfig struct {
	Addr              string        `yaml:"addr"`
	Password          string        `yaml:"password"`
	DB                int           `yaml:"db"`
	PoolSize          int           `yaml:"pool_size"`
	DialTimeout       time.Duration `yaml:"dial_timeout"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	HeartbeatTTL      time.Duration `yaml:"heartbeat_ttl"`
}

// FallbackConfig holds configuration for fallback mode when Redis is unavailable.
type FallbackConfig struct {
	Enabled           bool `yaml:"enabled"`
	LocalLimitDivisor int  `yaml:"local_limit_divisor"`
}

// Config is the root configuration structure.
type Config struct {
	Proxy    ProxyConfig    `yaml:"proxy"`
	Redis    RedisConfig    `yaml:"redis"`
	Fallback FallbackConfig `yaml:"fallback"`
	Buckets  []bucket.Bucket
}

// proxyFileConfig mirrors the YAML structure for the proxy config file.
type proxyFileConfig struct {
	Proxy    ProxyConfig    `yaml:"proxy"`
	Redis    RedisConfig    `yaml:"redis"`
	Fallback FallbackConfig `yaml:"fallback"`
}

// bucketsFileConfig mirrors the YAML structure for the buckets config file.
type bucketsFileConfig struct {
	Buckets []bucket.Bucket `yaml:"buckets"`
}

// Load reads and parses both proxy and buckets configuration files.
func Load(proxyConfigPath, bucketsConfigPath string) (*Config, error) {
	proxyData, err := os.ReadFile(proxyConfigPath)
	if err != nil {
		return nil, fmt.Errorf("reading proxy config %s: %w", proxyConfigPath, err)
	}

	var proxyFile proxyFileConfig
	if err := yaml.Unmarshal(proxyData, &proxyFile); err != nil {
		return nil, fmt.Errorf("parsing proxy config %s: %w", proxyConfigPath, err)
	}

	bucketsData, err := os.ReadFile(bucketsConfigPath)
	if err != nil {
		return nil, fmt.Errorf("reading buckets config %s: %w", bucketsConfigPath, err)
	}

	var bucketsFile bucketsFileConfig
	if err := yaml.Unmarshal(bucketsData, &bucketsFile); err != nil {
		return nil, fmt.Errorf("parsing buckets config %s: %w", bucketsConfigPath, err)
	}

	cfg := &Config{
		Proxy:    proxyFile.Proxy,
		Redis:    proxyFile.Redis,
		Fallback: proxyFile.Fallback,
		Buckets:  bucketsFile.Buckets,
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	cfg.applyDefaults()

	return cfg, nil
}

// validate checks mandatory fields.
func (c *Config) validate() error {
	if c.Proxy.ListenPort == 0 {
		return fmt.Errorf("proxy.listen_port is required")
	}
	if len(c.Buckets) == 0 {
		return fmt.Errorf("at least one bucket must be configured")
	}
	for i, b := range c.Buckets {
		if b.ID == "" {
			return fmt.Errorf("bucket[%d].id is required", i)
		}
		if b.Host == "" {
			return fmt.Errorf("bucket[%d].host is required", i)
		}
		if b.Port == 0 {
			return fmt.Errorf("bucket[%d].port is required", i)
		}
		if b.MaxConnections == 0 {
			return fmt.Errorf("bucket[%d].max_connections is required", i)
		}
	}
	return nil
}

// applyDefaults fills in reasonable defaults for unset optional fields.
func (c *Config) applyDefaults() {
	if c.Proxy.ListenAddr == "" {
		c.Proxy.ListenAddr = "0.0.0.0"
	}
	if c.Proxy.SessionTimeout == 0 {
		c.Proxy.SessionTimeout = 5 * time.Minute
	}
	if c.Proxy.IdleTimeout == 0 {
		c.Proxy.IdleTimeout = 60 * time.Second
	}
	if c.Proxy.QueueTimeout == 0 {
		c.Proxy.QueueTimeout = 30 * time.Second
	}
	if c.Proxy.MaxQueueSize == 0 {
		c.Proxy.MaxQueueSize = 1000
	}
	if c.Proxy.PinningMode == "" {
		c.Proxy.PinningMode = "transaction"
	}
	if c.Proxy.HealthCheckInterval == 0 {
		c.Proxy.HealthCheckInterval = 15 * time.Second
	}
	if c.Proxy.HealthCheckPort == 0 {
		c.Proxy.HealthCheckPort = 8080
	}
	if c.Proxy.MetricsPort == 0 {
		c.Proxy.MetricsPort = 9090
	}
	if c.Proxy.InstanceID == "" {
		hostname, _ := os.Hostname()
		c.Proxy.InstanceID = hostname
	}
	if c.Redis.Addr == "" {
		c.Redis.Addr = "redis:6379"
	}
	if c.Redis.PoolSize == 0 {
		c.Redis.PoolSize = 20
	}
	if c.Redis.DialTimeout == 0 {
		c.Redis.DialTimeout = 5 * time.Second
	}
	if c.Redis.ReadTimeout == 0 {
		c.Redis.ReadTimeout = 3 * time.Second
	}
	if c.Redis.WriteTimeout == 0 {
		c.Redis.WriteTimeout = 3 * time.Second
	}
	if c.Redis.HeartbeatInterval == 0 {
		c.Redis.HeartbeatInterval = 10 * time.Second
	}
	if c.Redis.HeartbeatTTL == 0 {
		c.Redis.HeartbeatTTL = 30 * time.Second
	}
	if c.Fallback.LocalLimitDivisor == 0 {
		c.Fallback.LocalLimitDivisor = 3
	}

	for i := range c.Buckets {
		if c.Buckets[i].MinIdle == 0 {
			c.Buckets[i].MinIdle = 2
		}
		if c.Buckets[i].MaxIdleTime == 0 {
			c.Buckets[i].MaxIdleTime = 5 * time.Minute
		}
		if c.Buckets[i].ConnectionTimeout == 0 {
			c.Buckets[i].ConnectionTimeout = 30 * time.Second
		}
		if c.Buckets[i].QueueTimeout == 0 {
			c.Buckets[i].QueueTimeout = c.Proxy.QueueTimeout
		}
	}
}

// BucketByID returns the bucket configuration for a given bucket ID.
func (c *Config) BucketByID(id string) (*bucket.Bucket, bool) {
	for i := range c.Buckets {
		if c.Buckets[i].ID == id {
			return &c.Buckets[i], true
		}
	}
	return nil, false
}

// BucketByDatabase returns the bucket configuration for a given database name.
// This is used by the TDS proxy to route connections based on the database name in Login7.
func (c *Config) BucketByDatabase(database string) (*bucket.Bucket, bool) {
	for i := range c.Buckets {
		if c.Buckets[i].Database == database {
			return &c.Buckets[i], true
		}
	}
	return nil, false
}
