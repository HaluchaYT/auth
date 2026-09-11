package storage

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/supabase/auth/internal/conf"
	"github.com/supabase/auth/internal/tenant"
)

// tenantPools holds one dedicated *Connection (i.e. one database/sql pool)
// per tenant schema, dialed lazily on first use and kept for the life of
// the process. Package-level so that every copy of the base Connection —
// and every API instance produced by a config reload — shares it.
//
// Why a pool per tenant instead of SET search_path per query?
//
// pop issues most reads outside any transaction and offers no per-query
// hook, so a session-level search_path cannot be set safely on a shared
// pool (it would leak to the next borrower). Pinning search_path in the
// DSN of a pool that only ever serves one tenant makes the scoping a
// property of the connection itself: nothing running on it can reach
// another tenant's schema, transaction or not.
var tenantPools = &tenantPoolCache{pools: make(map[string]*Connection)}

type tenantPoolCache struct {
	mu    sync.RWMutex
	pools map[string]*Connection // keyed by schema name
}

func (p *tenantPoolCache) get(ctx context.Context, base *conf.GlobalConfiguration, tc *tenant.Config) (*Connection, error) {
	if !isSafeIdentifier(tc.Schema) {
		return nil, tenant.ErrInvalidSlug
	}

	p.mu.RLock()
	conn, ok := p.pools[tc.Schema]
	p.mu.RUnlock()
	if ok {
		return conn, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if conn, ok := p.pools[tc.Schema]; ok {
		return conn, nil
	}

	cfg, err := tenantPoolConfig(base, tc.Schema)
	if err != nil {
		return nil, err
	}
	conn, err = DialContext(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("multitenant: dialing pool for schema %q: %w", tc.Schema, err)
	}
	logrus.WithFields(logrus.Fields{
		"component": "multitenant",
		"tenant":    tc.Slug,
		"schema":    tc.Schema,
		"pool_size": cfg.DB.MaxPoolSize,
	}).Info("opened tenant connection pool")

	p.pools[tc.Schema] = conn
	return conn, nil
}

// tenantPoolConfig derives the configuration for a tenant's pool from the
// base configuration:
//
//   - DB.URL gains a search_path startup parameter pinned to the tenant
//     schema (pgx forwards unknown URL parameters as runtime params).
//   - The pool is kept small; tenants are many and mostly idle.
//   - Driver instrumentation (tracing/metrics) is disabled for the derived
//     pool so the otel SQL driver and DB-stats meters are registered only
//     once, by the base connection. Request spans still flow via ctx.
func tenantPoolConfig(base *conf.GlobalConfiguration, schema string) (*conf.GlobalConfiguration, error) {
	cfg := *base // shallow copy; DBConfiguration etc. are value fields

	dsn, err := withSearchPath(base.DB.URL, schema)
	if err != nil {
		return nil, err
	}
	cfg.DB.URL = dsn

	size := base.MultiTenant.PoolSize
	if size <= 0 {
		size = 4
	}
	cfg.DB.MaxPoolSize = size
	cfg.DB.MaxIdlePoolSize = 1
	if cfg.DB.ConnMaxIdleTime == 0 {
		cfg.DB.ConnMaxIdleTime = 5 * time.Minute
	}
	cfg.DB.ConnPercentage = 0 // never let a tenant pool size itself off max_connections

	cfg.Tracing.Enabled = false
	cfg.Metrics.Enabled = false

	return &cfg, nil
}

// withSearchPath returns dsn with a search_path runtime parameter set to
// "<schema>, public, extensions". The tenant schema is first; public and
// extensions only so unqualified helper functions resolve. The shared
// upstream auth schema is deliberately absent — see tenantSearchPathSQL.
func withSearchPath(dsn, schema string) (string, error) {
	if !isSafeIdentifier(schema) {
		return "", tenant.ErrInvalidSlug
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("multitenant: parsing DB URL: %w", err)
	}
	q := u.Query()
	q.Set("search_path", schema+",public,extensions")
	u.RawQuery = q.Encode()
	return u.String(), nil
}
