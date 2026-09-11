package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/supabase/auth/internal/conf"
	"github.com/supabase/auth/internal/storage"
	"github.com/supabase/auth/internal/tenant"
)

// TestCrossTenantIsolation is the end-to-end proof that two tenants served by
// one process cannot see each other:
//
//   - the same email signs up successfully on both tenants
//   - a token minted by tenant one is rejected by tenant two
//   - each tenant's users table holds exactly its own user
//
// It needs a Postgres with the upstream auth schema migrated (the normal
// test DB) and is gated behind GOTRUE_MULTITENANT_TEST=1 so upstream's own
// CI matrix is unaffected. Run it with:
//
//	GOTRUE_MULTITENANT_TEST=1 go test ./internal/api -run TestCrossTenantIsolation -v
func TestCrossTenantIsolation(t *testing.T) {
	if os.Getenv("GOTRUE_MULTITENANT_TEST") != "1" {
		t.Skip("set GOTRUE_MULTITENANT_TEST=1 to run the multitenant integration test")
	}

	api, config, err := setupAPIForTestWithCallback(func(c *conf.GlobalConfiguration, conn *storage.Connection) {
		if c != nil {
			c.MultiTenant.Enabled = true
			c.MultiTenant.Strict = false
			c.Mailer.Autoconfirm = true
			c.External.Email.Enabled = true
		}
		if conn != nil {
			require.NoError(t, provisionTestTenants(conn, "t_one", "t_two"))
		}
	})
	require.NoError(t, err)
	require.NotNil(t, api.tenantStore, "multitenant should be initialised")
	_ = config

	const email = "alice@example.com"
	const password = "correct-horse-battery-staple"

	tok1 := signupAndToken(t, api, "t-one.local", email, password)
	tok2 := signupAndToken(t, api, "t-two.local", email, password)
	require.NotEmpty(t, tok1)
	require.NotEmpty(t, tok2)
	require.NotEqual(t, tok1, tok2)

	// Each token works on its own tenant …
	require.Equal(t, http.StatusOK, getUserStatus(t, api, "t-one.local", tok1))
	require.Equal(t, http.StatusOK, getUserStatus(t, api, "t-two.local", tok2))

	// … and is rejected by the other.
	require.Equal(t, http.StatusForbidden, getUserStatus(t, api, "t-two.local", tok1),
		"tenant one's token must not authenticate on tenant two")
	require.Equal(t, http.StatusForbidden, getUserStatus(t, api, "t-one.local", tok2),
		"tenant two's token must not authenticate on tenant one")

	// An unregistered subdomain never reaches a handler.
	rec := do(t, api, "nobody.local", http.MethodGet, "/health", nil, "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	// Row-level proof: each tenant schema holds exactly one alice.
	for _, schema := range []string{"t_one_auth", "t_two_auth"} {
		var n int
		err := api.db.RawQuery(fmt.Sprintf(`select count(*) from %q.users where email = ?`, schema), email).First(&n)
		require.NoError(t, err)
		require.Equal(t, 1, n, "schema %s should contain exactly one user", schema)
	}
}

// provisionTestTenants creates the control schema, clones the upstream auth
// tables into one schema per tenant and registers the tenants.
func provisionTestTenants(conn *storage.Connection, slugs ...string) error {
	stmts := []string{
		`create schema if not exists _control`,
		`create table if not exists _control._tenants (
			slug text primary key, schema_name text not null, jwt_secret text not null,
			jwt_issuer text not null, site_url text not null, redirect_urls text default '',
			smtp_host text not null default '', smtp_port integer not null default 587,
			smtp_user text not null default '', smtp_pass text not null default '',
			smtp_from text not null default '', created_at timestamptz not null default now(),
			updated_at timestamptz not null default now())`,
	}
	for _, s := range stmts {
		if err := conn.RawQuery(s).Exec(); err != nil {
			return err
		}
	}

	type pgTable struct {
		Tablename string `db:"tablename"`
	}
	var rows []pgTable
	if err := conn.RawQuery(`select tablename from pg_tables where schemaname = 'auth'`).All(&rows); err != nil {
		return err
	}
	tables := make([]string, 0, len(rows))
	for _, r := range rows {
		tables = append(tables, r.Tablename)
	}

	for _, slug := range slugs {
		schema := slug + "_auth"
		if err := conn.RawQuery(fmt.Sprintf(`create schema if not exists %q`, schema)).Exec(); err != nil {
			return err
		}
		for _, tbl := range tables {
			q := fmt.Sprintf(`create table if not exists %q.%q (like auth.%q including all)`, schema, tbl, tbl)
			if err := conn.RawQuery(q).Exec(); err != nil {
				return err
			}
		}
		secret, err := tenant.GenerateJWTSecret()
		if err != nil {
			return err
		}
		// slugs use dashes in DNS ("t-one"), schemas use underscores.
		dnsSlug := strings.ReplaceAll(slug, "_", "-")
		q := `insert into _control._tenants (slug, schema_name, jwt_secret, jwt_issuer, site_url)
		      values (?, ?, ?, ?, ?) on conflict (slug) do update set schema_name = excluded.schema_name`
		if err := conn.RawQuery(q, dnsSlug, schema, secret, "https://"+dnsSlug+".local", "https://"+dnsSlug+".local").Exec(); err != nil {
			return err
		}
	}
	return nil
}

func do(t *testing.T, api *API, host, method, path string, body any, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, "http://"+host+path, &buf)
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	api.handler.ServeHTTP(rec, req)
	return rec
}

func signupAndToken(t *testing.T, api *API, host, email, password string) string {
	t.Helper()
	rec := do(t, api, host, http.MethodPost, "/signup", map[string]any{
		"email": email, "password": password,
	}, "")
	require.Equal(t, http.StatusOK, rec.Code, "signup on %s: %s", host, rec.Body.String())

	var resp struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	return resp.AccessToken
}

func getUserStatus(t *testing.T, api *API, host, bearer string) int {
	t.Helper()
	return do(t, api, host, http.MethodGet, "/user", nil, bearer).Code
}
