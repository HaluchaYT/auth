package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/supabase/auth/internal/tenant"
)

// applyTenantSearchPath is called at the top of every transaction opened
// through Connection.Transaction. It is the single point where tenant
// isolation is enforced at the database layer:
//
//   - tenant in ctx        → SET LOCAL search_path TO "<tenant schema>", ...
//   - no tenant, system    → run unscoped (operator background paths)
//   - no tenant, strict    → refuse with tenant.ErrMissingTenant
//   - no tenant, lenient   → run unscoped (upstream behaviour)
//
// Because SET LOCAL is transaction-scoped, Postgres resets it at COMMIT or
// ROLLBACK and it can never leak to the next request that reuses the same
// pooled connection.
func applyTenantSearchPath(ctx context.Context, tx *Connection) error {
	if cfg, ok := tenant.FromContext(ctx); ok {
		// Defence in depth: the middleware validated the slug, and the
		// control table has a CHECK on schema_name, but never build SQL
		// from an identifier we haven't re-validated here.
		if !isSafeIdentifier(cfg.Schema) {
			return tenant.ErrInvalidSlug
		}
		if err := tx.RawQuery(tenantSearchPathSQL(cfg.Schema)).Exec(); err != nil {
			return fmt.Errorf("tenant: SET LOCAL search_path: %w", err)
		}
		return nil
	}
	if tenant.IsSystem(ctx) {
		// Operator background path. Pin it to the configured system schema
		// when multitenant is on, so it never depends on the connection's
		// default search_path (which a hardened deployment may point at a
		// decoy schema — see MULTITENANT.md → Hardening).
		if tenant.SystemSchema != "" {
			if !isSafeIdentifier(tenant.SystemSchema) {
				return tenant.ErrInvalidSlug
			}
			if err := tx.RawQuery(tenantSearchPathSQL(tenant.SystemSchema)).Exec(); err != nil {
				return fmt.Errorf("tenant: SET LOCAL search_path (system): %w", err)
			}
		}
		return nil
	}
	if tenant.Strict {
		return tenant.ErrMissingTenant
	}
	return nil
}

// tenantSearchPathSQL builds the SET LOCAL statement for a validated schema.
//
// The tenant schema comes first. public and extensions follow only so that
// unqualified helper functions (gen_random_uuid, crypt, …) still resolve.
// The shared upstream auth schema is deliberately NOT on the path: a table
// missing from a tenant's schema must fail loudly, never fall through to
// another realm's rows. Non-existent schemas on the path are ignored by
// Postgres, so listing extensions is safe where it does not exist.
func tenantSearchPathSQL(schema string) string {
	return "SET LOCAL search_path TO " + pqQuoteIdent(schema) + ", public, extensions"
}

// WithTenantTransaction is the explicit form of the hook: it refuses to run
// unless a tenant is resolved (regardless of strict mode) and then runs fn
// inside a tenant-scoped transaction. Prefer this in new code paths that
// must never run unscoped; existing paths get the same behaviour implicitly
// through Connection.Transaction.
func (c *Connection) WithTenantTransaction(ctx context.Context, fn func(*Connection) error) error {
	cfg, ok := tenant.FromContext(ctx)
	if !ok {
		return tenant.ErrMissingTenant
	}
	if !isSafeIdentifier(cfg.Schema) {
		return tenant.ErrInvalidSlug
	}
	return c.WithContext(ctx).Transaction(fn)
}

// WithTenantSqlDB is the lower-level variant for code that speaks raw
// database/sql rather than pop. txFn receives an *sql.Tx that has already
// run SET LOCAL search_path; it must not Commit or Rollback itself.
func WithTenantSqlDB(ctx context.Context, db *sql.DB, txFn func(*sql.Tx) error) error {
	cfg, ok := tenant.FromContext(ctx)
	if !ok {
		return tenant.ErrMissingTenant
	}
	if !isSafeIdentifier(cfg.Schema) {
		return tenant.ErrInvalidSlug
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, tenantSearchPathSQL(cfg.Schema)); err != nil {
		return fmt.Errorf("tenant: SET LOCAL search_path: %w", err)
	}
	if err := txFn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// isSafeIdentifier accepts only what Postgres parses as a plain unquoted
// identifier: [a-z_][a-z0-9_]*, at most 63 bytes. Tenant schema names are
// snake_case by construction (see the CHECK constraint on
// _control._tenants.schema_name), so anything else is an error, not a
// quoting problem.
func isSafeIdentifier(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			continue
		case r == '_':
			continue
		case i > 0 && r >= '0' && r <= '9':
			continue
		default:
			return false
		}
	}
	return true
}

// pqQuoteIdent double-quotes an identifier for use in SQL. The input has
// already passed isSafeIdentifier; quoting is belt-and-braces.
func pqQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
