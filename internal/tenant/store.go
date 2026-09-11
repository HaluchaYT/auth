package tenant

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// Store loads tenant configs from a Postgres control schema and caches them
// in memory. Callers hit Load() on every request, so the cache is what keeps
// latency down. Cache entries expire after DefaultCacheTTL; the next lookup
// pulls a fresh row and refreshes the cache slot.
//
// The store is safe for concurrent use.
type Store struct {
	db         *sql.DB
	controlSql controlQueries
	ttl        time.Duration

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	cfg       *Config
	expiresAt time.Time
}

// DefaultCacheTTL is the default lifetime of a cached tenant config. Long
// enough that we don't hammer the control DB, short enough that config edits
// take effect within a few minutes.
const DefaultCacheTTL = 5 * time.Minute

// NewStore constructs a Store backed by db. The control schema (see the SQL
// migration under db/migrations/) must already exist.
func NewStore(db *sql.DB, opts ...StoreOption) *Store {
	s := &Store{
		db:         db,
		controlSql: defaultControlQueries(),
		ttl:        DefaultCacheTTL,
		cache:      make(map[string]cacheEntry),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// StoreOption configures a Store during construction.
type StoreOption func(*Store)

// WithCacheTTL overrides the cache lifetime for testing or ops tuning.
func WithCacheTTL(d time.Duration) StoreOption {
	return func(s *Store) { s.ttl = d }
}

// Load returns the tenant config for slug. Cache-through: hits memory first,
// falls back to the DB, records the result. Returns ErrUnknownTenant if the
// slug has no row.
func (s *Store) Load(ctx context.Context, slug string) (*Config, error) {
	// Fast path — read-locked cache hit.
	s.mu.RLock()
	entry, ok := s.cache[slug]
	s.mu.RUnlock()
	if ok && time.Now().Before(entry.expiresAt) {
		return entry.cfg, nil
	}

	// Slow path — hit the control table.
	cfg, err := s.fetch(ctx, slug)
	if err != nil {
		return nil, err
	}

	// Fill the cache. Last-writer wins is fine here — every writer produces
	// the same value modulo timing.
	s.mu.Lock()
	s.cache[slug] = cacheEntry{cfg: cfg, expiresAt: time.Now().Add(s.ttl)}
	s.mu.Unlock()
	return cfg, nil
}

// Invalidate removes a tenant from the cache, forcing the next Load to
// re-read from Postgres. Called by admin endpoints after updating a config.
func (s *Store) Invalidate(slug string) {
	s.mu.Lock()
	delete(s.cache, slug)
	s.mu.Unlock()
}

// InvalidateAll drops every cached entry. Useful for tests and hot config
// reloads.
func (s *Store) InvalidateAll() {
	s.mu.Lock()
	s.cache = make(map[string]cacheEntry)
	s.mu.Unlock()
}

// fetch reads a single tenant row from the control table and inflates it
// into a Config. Returns ErrUnknownTenant on no rows.
func (s *Store) fetch(ctx context.Context, slug string) (*Config, error) {
	row := s.db.QueryRowContext(ctx, s.controlSql.selectBySlug, slug)

	var cfg Config
	var redirectCSV sql.NullString
	err := row.Scan(
		&cfg.Slug,
		&cfg.Schema,
		&cfg.JWTSecret,
		&cfg.JWTIssuer,
		&cfg.SiteURL,
		&redirectCSV,
		&cfg.SMTPHost,
		&cfg.SMTPPort,
		&cfg.SMTPUser,
		&cfg.SMTPPass,
		&cfg.SMTPFrom,
		&cfg.CreatedAt,
		&cfg.UpdatedAt,
	)
	switch {
	case err == sql.ErrNoRows:
		return nil, ErrUnknownTenant
	case err != nil:
		return nil, fmt.Errorf("tenant.Store.fetch(%q): %w", slug, err)
	}

	if redirectCSV.Valid && redirectCSV.String != "" {
		cfg.RedirectURLs = splitCSV(redirectCSV.String)
	}
	return &cfg, nil
}

// controlQueries centralizes the SQL text so tests can substitute their own.
type controlQueries struct {
	selectBySlug string
}

func defaultControlQueries() controlQueries {
	return controlQueries{
		selectBySlug: `
			SELECT slug, schema_name, jwt_secret, jwt_issuer, site_url,
			       redirect_urls, smtp_host, smtp_port, smtp_user, smtp_pass, smtp_from,
			       created_at, updated_at
			FROM _control._tenants
			WHERE slug = $1
		`,
	}
}

func splitCSV(s string) []string {
	out := []string{}
	current := ""
	for _, r := range s {
		if r == ',' {
			if current != "" {
				out = append(out, current)
			}
			current = ""
			continue
		}
		current += string(r)
	}
	if current != "" {
		out = append(out, current)
	}
	return out
}
