// Package bucket defines the bucket model and configuration structures.
// A bucket represents a logical grouping of tenants mapped to a single RDS SQL Server instance.
package bucket

import "time"

// Bucket represents a logical bucket mapped to a single SQL Server RDS instance.
type Bucket struct {
	ID               string        `yaml:"id"`
	Host             string        `yaml:"host"`
	Port             int           `yaml:"port"`
	Database         string        `yaml:"database"`
	Username         string        `yaml:"username"`
	Password         string        `yaml:"password"`
	MaxConnections   int           `yaml:"max_connections"`
	MinIdle          int           `yaml:"min_idle"`
	MaxIdleTime      time.Duration `yaml:"max_idle_time"`
	ConnectionTimeout time.Duration `yaml:"connection_timeout"`
	QueueTimeout     time.Duration `yaml:"queue_timeout"`
}

// DSN returns the SQL Server connection string for this bucket.
func (b *Bucket) DSN() string {
	return "sqlserver://" + b.Username + ":" + b.Password +
		"@" + b.Host + ":" + itoa(b.Port) +
		"?database=" + b.Database +
		"&connection+timeout=" + itoa(int(b.ConnectionTimeout.Seconds()))
}

// Addr returns the host:port address of the SQL Server instance.
func (b *Bucket) Addr() string {
	return b.Host + ":" + itoa(b.Port)
}

// itoa converts an integer to string without importing strconv at package level.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	if n < 0 {
		return "-" + itoa(-n)
	}
	digits := make([]byte, 0, 10)
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
