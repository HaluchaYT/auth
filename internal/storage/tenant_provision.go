package storage

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/gobuffalo/pop/v6"
	"github.com/sirupsen/logrus"

	"github.com/supabase/auth/internal/conf"
	"github.com/supabase/auth/internal/tenant"
)

// Auto-provisioning lets a tenant be registered from the dashboard alone:
// insert a row in _control._tenants, and on the tenant's first request the
// service creates its schema and runs the upstream migrations against it
// (namespace-templated, so constraint names and every later upgrade match
// the shared schema exactly). Enabled with GOTRUE_MULTITENANT_AUTO_PROVISION.

var (
	tenantMigrationsFS   fs.FS
	tenantMigrationsPath string
)

// SetTenantMigrations registers the embedded upstream migrations (the same
// embed.FS `auth migrate` uses). Called once from main.
func SetTenantMigrations(f fs.FS) { tenantMigrationsFS = f }

// SetTenantMigrationsPath registers an on-disk migrations directory. Used
// by tests, where an embed directive cannot reach ../../migrations.
func SetTenantMigrationsPath(dir string) { tenantMigrationsPath = dir }

// tenantSchemaProvisioned reports whether schema already holds the auth
// tables (users is created by the first migration).
func tenantSchemaProvisioned(conn *Connection, schema string) (bool, error) {
	var n int
	err := conn.RawQuery(
		`select count(*) from pg_tables where schemaname = ? and tablename = 'users'`, schema,
	).First(&n)
	return n > 0, err
}

// provisionTenantSchema creates schema (if needed) and applies the upstream
// migrations to it. conn is the tenant's own pool: its DSN already pins
// search_path to schema, so the migrator's schema_migrations table lands
// there too. Callers serialize this (tenantPoolCache.get holds the lock).
func provisionTenantSchema(base *conf.GlobalConfiguration, conn *Connection, schema string) error {
	if tenantMigrationsFS == nil && tenantMigrationsPath == "" {
		return errors.New("multitenant: auto-provision is enabled but no migrations are registered")
	}
	if !isSafeIdentifier(schema) {
		return tenant.ErrInvalidSlug
	}
	log := logrus.WithFields(logrus.Fields{"component": "multitenant", "schema": schema})

	// 1. The schema. CREATE SCHEMA ignores search_path; the auth role owns
	//    what it creates, matching how `auth` itself is set up.
	if err := conn.RawQuery("create schema if not exists " + pqQuoteIdent(schema)).Exec(); err != nil {
		return fmt.Errorf("multitenant: create schema %q: %w", schema, err)
	}

	// 2. A dedicated pop connection for the migrator, exactly like
	//    `auth migrate`: namespace templated, search_path pinned.
	dsn, err := withSearchPath(base.DB.URL, schema)
	if err != nil {
		return err
	}
	dialect := base.DB.Driver
	if dialect == "" {
		dialect = "postgres"
	}
	deets := &pop.ConnectionDetails{
		Dialect: dialect,
		URL:     dsn,
		Options: map[string]string{
			"migration_table_name": "schema_migrations",
			"Namespace":            schema,
		},
	}
	db, err := pop.NewConnection(deets)
	if err != nil {
		return fmt.Errorf("multitenant: migrator connection for %q: %w", schema, err)
	}
	if err := db.Open(); err != nil {
		return fmt.Errorf("multitenant: opening migrator connection for %q: %w", schema, err)
	}
	defer db.Close()

	var mig pop.Migrator
	if tenantMigrationsFS != nil {
		box, err := pop.NewMigrationBox(tenantMigrationsFS, db)
		if err != nil {
			return fmt.Errorf("multitenant: loading embedded migrations: %w", err)
		}
		mig = box.Migrator
	} else {
		fm, err := pop.NewFileMigrator(tenantMigrationsPath, db)
		if err != nil {
			return fmt.Errorf("multitenant: loading migrations from %s: %w", tenantMigrationsPath, err)
		}
		mig = fm.Migrator
	}
	mig.SchemaPath = "" // never dump schema.sql from a request path

	log.Info("provisioning tenant schema with upstream migrations")
	count, err := mig.UpTo(0)
	if err != nil {
		return fmt.Errorf("multitenant: migrating %q: %w", schema, err)
	}
	log.WithField("count", count).Info("tenant schema provisioned")
	return nil
}
