// Package bucket define o modelo de bucket e estruturas de configuração.
// Um bucket representa um agrupamento lógico de tenants mapeado para uma única instância RDS SQL Server.
package bucket

import "time"

// Bucket representa um bucket lógico mapeado para uma única instância RDS SQL Server.
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

// DSN retorna a string de conexão do SQL Server para este bucket.
func (b *Bucket) DSN() string {
	return "sqlserver://" + b.Username + ":" + b.Password +
		"@" + b.Host + ":" + itoa(b.Port) +
		"?database=" + b.Database +
		"&connection+timeout=" + itoa(int(b.ConnectionTimeout.Seconds()))
}

// Addr retorna o endereço host:port da instância SQL Server.
func (b *Bucket) Addr() string {
	return b.Host + ":" + itoa(b.Port)
}

// itoa converte um inteiro para string sem importar strconv no nível do pacote.
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
