package proxy

import (
	"fmt"
	"log"
	"strings"

	"github.com/joao-brasil/poc-connection-pooling/internal/config"
	"github.com/joao-brasil/poc-connection-pooling/internal/tds"
	"github.com/joao-brasil/poc-connection-pooling/pkg/bucket"
)

// ── Connection Router ───────────────────────────────────────────────────
//
// O router mapeia um pacote Login7 para um bucket de destino. Estratégias de roteamento:
//
// 1. Por nome do banco   — Login7.Database → bucket com database correspondente
// 2. Por nome do servidor — Login7.ServerName → ID do bucket
// 3. Por nome de usuário  — Login7.UserName → bucket com username correspondente
// 4. Primeiro match vence — fallback: se existir apenas um bucket, usá-lo
//
// Para a POC, todos os buckets compartilham o mesmo nome de banco ("tenant_db"), então
// usamos nome do servidor ou username como chaves de roteamento alternativas.

// Router resolve um pacote Login7 para um bucket de destino.
type Router struct {
	cfg *config.Config

	// byDatabase mapeia nome do banco → bucket (primeiro match vence).
	byDatabase map[string]*bucket.Bucket

	// byServerName mapeia alias de nome do servidor → bucket.
	byServerName map[string]*bucket.Bucket

	// byHost mapeia host:port → bucket.
	byHost map[string]*bucket.Bucket

	// byID mapeia ID do bucket → bucket para lookup direto.
	byID map[string]*bucket.Bucket

	// defaultBucket é usado quando há apenas um bucket ou nenhum match de roteamento.
	defaultBucket *bucket.Bucket
}

// NewRouter cria um Router a partir da configuração.
func NewRouter(cfg *config.Config) *Router {
	r := &Router{
		cfg:          cfg,
		byDatabase:   make(map[string]*bucket.Bucket),
		byServerName: make(map[string]*bucket.Bucket),
		byHost:       make(map[string]*bucket.Bucket),
		byID:         make(map[string]*bucket.Bucket),
	}

	// Construir mapas de lookup.
	seenDBs := make(map[string]int) // rastrear duplicatas
	for i := range cfg.Buckets {
		b := &cfg.Buckets[i]
		r.byID[b.ID] = b
		r.byHost[b.Addr()] = b
		seenDBs[b.Database]++

		// Mapear ID do bucket como alias de nome de servidor (ex: "bucket-001").
		r.byServerName[strings.ToLower(b.ID)] = b

		// Também mapear o host como alias de nome de servidor.
		r.byServerName[strings.ToLower(b.Host)] = b
	}

	// Só preencher byDatabase se nomes de banco forem únicos entre buckets.
	for i := range cfg.Buckets {
		b := &cfg.Buckets[i]
		if seenDBs[b.Database] == 1 {
			r.byDatabase[strings.ToLower(b.Database)] = b
		}
	}

	// Se houver apenas um bucket, definir como padrão.
	if len(cfg.Buckets) == 1 {
		r.defaultBucket = &cfg.Buckets[0]
	}

	log.Printf("[router] Initialized: %d buckets, %d unique databases, %d server aliases",
		len(cfg.Buckets), len(r.byDatabase), len(r.byServerName))

	return r
}

// Route resolve um pacote Login7 para um bucket de destino.
// Retorna o bucket e nil de erro, ou nil e um erro se nenhuma rota foi encontrada.
func (r *Router) Route(login7 *tds.Login7Info) (*bucket.Bucket, error) {
	// Estratégia 1: Rotear por nome do servidor (mais explícito).
	// O cliente pode definir o nome do servidor como o ID do bucket para rotear explicitamente.
	if login7.ServerName != "" {
		serverLower := strings.ToLower(login7.ServerName)
		if b, ok := r.byServerName[serverLower]; ok {
			log.Printf("[router] Routed by server name %q → bucket %s", login7.ServerName, b.ID)
			return b, nil
		}

		// Tentar fazer match do nome do servidor como ID do bucket diretamente.
		if b, ok := r.byID[login7.ServerName]; ok {
			log.Printf("[router] Routed by bucket ID %q → bucket %s", login7.ServerName, b.ID)
			return b, nil
		}
	}

	// Estratégia 2: Rotear por nome do banco (se único).
	if login7.Database != "" {
		dbLower := strings.ToLower(login7.Database)
		if b, ok := r.byDatabase[dbLower]; ok {
			log.Printf("[router] Routed by database %q → bucket %s", login7.Database, b.ID)
			return b, nil
		}
	}

	// Estratégia 3: Rotear por match de username.
	if login7.UserName != "" {
		for i := range r.cfg.Buckets {
			b := &r.cfg.Buckets[i]
			if strings.EqualFold(b.Username, login7.UserName) {
				log.Printf("[router] Routed by username %q → bucket %s", login7.UserName, b.ID)
				return b, nil
			}
		}
	}

	// Estratégia 4: Bucket padrão (setup de bucket único).
	if r.defaultBucket != nil {
		log.Printf("[router] Routed to default bucket %s", r.defaultBucket.ID)
		return r.defaultBucket, nil
	}

	return nil, fmt.Errorf("no route found for login7: server=%q, database=%q, user=%q",
		login7.ServerName, login7.Database, login7.UserName)
}
