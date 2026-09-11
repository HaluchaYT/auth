package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/supabase/auth/internal/conf"
	"github.com/supabase/auth/internal/observability"
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

	var provisionErr error
	api, config, err := setupAPIForTestWithCallback(func(c *conf.GlobalConfiguration, conn *storage.Connection) {
		if c != nil {
			c.MultiTenant.Enabled = true
			c.MultiTenant.Strict = false
			c.Mailer.Autoconfirm = true
			c.External.Email.Enabled = true
		}
		if conn != nil {
			provisionErr = provisionTestTenants(conn, "t_one", "t_two")
		}
	})
	require.NoError(t, err)
	if errors.Is(provisionErr, errTenantSchemaMissing) {
		t.Skipf("%v — provision t_one_auth/t_two_auth with the upstream migrations first (see provisionTestTenants)", provisionErr)
	}
	require.NoError(t, provisionErr)
	require.NotNil(t, api.tenantStore, "multitenant should be initialised")

	// Surface the API's internal error logs (a 500 body is deliberately opaque).
	logrus.SetOutput(os.Stderr)
	logrus.SetLevel(logrus.DebugLevel)

	// ---- Diagnostics: prove tenant routing at the storage layer before HTTP ----
	tcfg, err := api.tenantStore.Load(context.Background(), "t-one")
	require.NoError(t, err, "load tenant t-one from registry")
	require.Equal(t, "t_one_auth", tcfg.Schema)

	tctx := tenant.WithConfig(context.Background(), tcfg)
	tdb := api.db.WithContext(tctx)

	// (1) a NON-transactional query must already be pinned by the pool DSN
	var sp string
	require.NoError(t, tdb.RawQuery("select current_setting('search_path')").First(&sp),
		"non-transactional query on tenant connection")
	require.Contains(t, sp, "t_one_auth", "tenant pool must pin search_path; got %q", sp)

	// (2) a transactional query runs under the hook
	var n int
	require.NoError(t, tdb.Transaction(func(tx *storage.Connection) error {
		return tx.RawQuery("select count(*) from users").First(&n)
	}), "transactional query on tenant connection")
	require.Equal(t, 0, n, "fresh tenant schema should have no users")

	// (3) the cloned schema accepts an insert
	require.NoError(t, tdb.Transaction(func(tx *storage.Connection) error {
		return tx.RawQuery(`insert into users (id, instance_id, aud, role, email, created_at, updated_at)
			values (gen_random_uuid(), '00000000-0000-0000-0000-000000000000', ?, 'authenticated', 'probe@example.com', now(), now())`,
			config.JWT.Aud).Exec()
	}), "raw insert into tenant users table")
	require.NoError(t, tdb.Transaction(func(tx *storage.Connection) error {
		return tx.RawQuery("delete from users where email = 'probe@example.com'").Exec()
	}))

	// (4) call the Signup handler directly, through the same logger and
	// external-host middlewares the router applies, so the underlying error
	// (or panic) is visible instead of an opaque 500.
	func() {
		defer func() {
			if rvr := recover(); rvr != nil {
				t.Fatalf("direct Signup panicked: %v\n%s", rvr, debug.Stack())
			}
		}()
		req := httptest.NewRequest(http.MethodPost, "http://t-one.local/signup",
			strings.NewReader(`{"email":"direct@example.com","password":"correct-horse-battery-staple"}`))
		req.Host = "t-one.local"
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(tenant.WithConfig(req.Context(), tcfg))
		rec := httptest.NewRecorder()

		var herr error
		logged := observability.NewStructuredLogger(logrus.StandardLogger(), config)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx2, e := api.isValidExternalHost(w, r)
				if e != nil {
					herr = fmt.Errorf("isValidExternalHost: %w", e)
					return
				}
				herr = api.Signup(w, r.WithContext(ctx2))
			}))
		logged.ServeHTTP(rec, req)
		require.NoError(t, herr, "direct Signup call (status %d, body %s)", rec.Code, rec.Body.String())
		require.Equal(t, http.StatusOK, rec.Code, "direct Signup body: %s", rec.Body.String())
	}()

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

// errTenantSchemaMissing signals that the tenant schemas have not been
// provisioned with the upstream migrations. The test skips with
// instructions rather than failing — cloning tables in-test is not an
// option because LIKE renames constraints the service relies on.
var errTenantSchemaMissing = errors.New("tenant schema not provisioned")

// provisionTestTenants creates the control schema and registers the
// tenants. The tenant schemas themselves must already be migrated:
//
//	psql "$PG_SUPERUSER_URL" -c 'create schema if not exists t_one_auth authorization supabase_auth_admin'
//	sed -e 's|^DB_NAMESPACE=.*|DB_NAMESPACE="t_one_auth"|' \
//	    -e 's|^DATABASE_URL=.*|DATABASE_URL="postgres://supabase_auth_admin:root@localhost:5432/postgres?search_path=t_one_auth"|' \
//	    hack/test.env > /tmp/tenant-t_one_auth.env
//	go run main.go migrate -c /tmp/tenant-t_one_auth.env
//
// (and the same for t_two_auth) — see .github/workflows/tenant.yml.
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

	for _, slug := range slugs {
		schema := slug + "_auth"

		var n int
		if err := conn.RawQuery(
			`select count(*) from pg_tables where schemaname = ? and tablename = 'users'`, schema,
		).First(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: %s", errTenantSchemaMissing, schema)
		}

		// Keep re-runs idempotent on a persistent local database.
		for _, tbl := range []string{"users"} {
			q := fmt.Sprintf(`delete from %q.%q where email in ('alice@example.com','direct@example.com','probe@example.com')`, schema, tbl)
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
