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
// The router maps a Login7 packet to a destination bucket. Routing strategies:
//
// 1. By database name   — Login7.Database → bucket with matching database
// 2. By server name     — Login7.ServerName → bucket ID
// 3. By username        — Login7.UserName → bucket with matching username
// 4. First match wins   — fallback: if only one bucket exists, use it
//
// For the POC, all buckets share the same database name ("tenant_db"), so
// we use server name or username as alternative routing keys.

// Router resolves a Login7 packet to a destination bucket.
type Router struct {
	cfg *config.Config

	// byDatabase maps database name → bucket (first match wins).
	byDatabase map[string]*bucket.Bucket

	// byServerName maps server name alias → bucket.
	byServerName map[string]*bucket.Bucket

	// byHost maps host:port → bucket.
	byHost map[string]*bucket.Bucket

	// byID maps bucket ID → bucket for direct lookup.
	byID map[string]*bucket.Bucket

	// defaultBucket is used when there's only one bucket or no routing match.
	defaultBucket *bucket.Bucket
}

// NewRouter creates a Router from the configuration.
func NewRouter(cfg *config.Config) *Router {
	r := &Router{
		cfg:          cfg,
		byDatabase:   make(map[string]*bucket.Bucket),
		byServerName: make(map[string]*bucket.Bucket),
		byHost:       make(map[string]*bucket.Bucket),
		byID:         make(map[string]*bucket.Bucket),
	}

	// Build lookup maps.
	seenDBs := make(map[string]int) // track duplicates
	for i := range cfg.Buckets {
		b := &cfg.Buckets[i]
		r.byID[b.ID] = b
		r.byHost[b.Addr()] = b
		seenDBs[b.Database]++

		// Map bucket ID as a server name alias (e.g., "bucket-001").
		r.byServerName[strings.ToLower(b.ID)] = b

		// Also map the host as a server name alias.
		r.byServerName[strings.ToLower(b.Host)] = b
	}

	// Only populate byDatabase if database names are unique across buckets.
	for i := range cfg.Buckets {
		b := &cfg.Buckets[i]
		if seenDBs[b.Database] == 1 {
			r.byDatabase[strings.ToLower(b.Database)] = b
		}
	}

	// If there's only one bucket, set it as default.
	if len(cfg.Buckets) == 1 {
		r.defaultBucket = &cfg.Buckets[0]
	}

	log.Printf("[router] Initialized: %d buckets, %d unique databases, %d server aliases",
		len(cfg.Buckets), len(r.byDatabase), len(r.byServerName))

	return r
}

// Route resolves a Login7 packet to a destination bucket.
// Returns the bucket and nil error, or nil and an error if no route found.
func (r *Router) Route(login7 *tds.Login7Info) (*bucket.Bucket, error) {
	// Strategy 1: Route by server name (most explicit).
	// The client can set the server name to the bucket ID to route explicitly.
	if login7.ServerName != "" {
		serverLower := strings.ToLower(login7.ServerName)
		if b, ok := r.byServerName[serverLower]; ok {
			log.Printf("[router] Routed by server name %q → bucket %s", login7.ServerName, b.ID)
			return b, nil
		}

		// Try matching server name as bucket ID directly.
		if b, ok := r.byID[login7.ServerName]; ok {
			log.Printf("[router] Routed by bucket ID %q → bucket %s", login7.ServerName, b.ID)
			return b, nil
		}
	}

	// Strategy 2: Route by database name (if unique).
	if login7.Database != "" {
		dbLower := strings.ToLower(login7.Database)
		if b, ok := r.byDatabase[dbLower]; ok {
			log.Printf("[router] Routed by database %q → bucket %s", login7.Database, b.ID)
			return b, nil
		}
	}

	// Strategy 3: Route by username matching.
	if login7.UserName != "" {
		for i := range r.cfg.Buckets {
			b := &r.cfg.Buckets[i]
			if strings.EqualFold(b.Username, login7.UserName) {
				log.Printf("[router] Routed by username %q → bucket %s", login7.UserName, b.ID)
				return b, nil
			}
		}
	}

	// Strategy 4: Default bucket (single-bucket setup).
	if r.defaultBucket != nil {
		log.Printf("[router] Routed to default bucket %s", r.defaultBucket.ID)
		return r.defaultBucket, nil
	}

	return nil, fmt.Errorf("no route found for login7: server=%q, database=%q, user=%q",
		login7.ServerName, login7.Database, login7.UserName)
}
