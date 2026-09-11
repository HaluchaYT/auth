package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/supabase/auth/internal/tenant"
)

// WithTenantTransaction runs fn inside a Postgres transaction whose
// search_path is scoped to the resolved tenant's schema. This is the ONE
// entry point every model call MUST use — never touch the naked
// Connection.Transaction() from a request-scoped code path.
//
// The tenant is pulled from ctx via tenant.FromContext. If ctx has no
// tenant, this returns tenant.ErrMissingTenant BEFORE opening any DB
// resource. This is by design: a query without a tenant boundary must
// not be able to reach Postgres.
//
// The tenant-scoped search_path is set with SET LOCAL, which Postgres
// automatically clears at COMMIT or ROLLBACK. That means the setting can
// never leak to the next request that reuses the same pooled connection.
func (c *Connection) WithTenantTransaction(ctx context.Context, fn func(*Connection) error) error {
	cfg, ok := tenant.FromContext(ctx)
	if !ok {
		return tenant.ErrMissingTenant
	}
	// Defence-in-depth: never build SQL from an unvalidated schema name.
	// The middleware already validates against slugPattern, but revalidate
	// so a code path that constructs a Config without the middleware
	// still can't inject.
	if !isSafeIdentifier(cfg.Schema) {
		return tenant.ErrInvalidSlug
	}

	cx := c.WithContext(ctx)
	return cx.Transaction(func(tx *Connection) error {
		if err := tx.RawQuery(fmt.Sprintf("SET LOCAL search_path TO %s", pqQuoteIdent(cfg.Schema))).Exec(); err != nil {
			return fmt.Errorf("tenant hook: SET LOCAL search_path: %w", err)
		}
		return fn(tx)
	})
}

// WithTenantSqlDB is the lower-level variant that hands the caller a
// *sql.DB-style handle already committed to a tenant transaction. Used by
// code paths that construct queries via raw SQL rather than pop models.
//
// The txFn receives an *sql.Tx that has already run SET LOCAL search_path.
// The caller MUST NOT commit or roll back the tx directly — return an error
// (or nil) from txFn and this helper does the finalization.
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
	// The rollback here is a no-op if the caller returns nil and we commit
	// successfully below; deferred Rollback after Commit returns
	// sql.ErrTxDone which is safe to ignore.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL search_path TO %s", pqQuoteIdent(cfg.Schema))); err != nil {
		return fmt.Errorf("tenant hook: SET LOCAL search_path: %w", err)
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

// isSafeIdentifier accepts only what Postgres would parse as an unquoted
// identifier without needing quoting: [a-z_][a-z0-9_]*. Kept restrictive
// on purpose — the tenant slug regex allows dashes but we translate schema
// names to snake_case in the control table, so schema names never contain
// dashes anyway.
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

// pqQuoteIdent wraps a validated identifier in double quotes for use in SQL.
// We already validated with isSafeIdentifier, so this is belt-and-braces —
// the resulting string is safe to interpolate into a SET LOCAL statement.
func pqQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
