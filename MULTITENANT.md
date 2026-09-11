# Multi-tenant fork of `supabase/auth`

One auth container, many fully isolated auth realms. Each tenant gets its
own Postgres schema (users, sessions, tokens …), its own JWT signing key,
its own SMTP identity and site URL, and its own OAuth app credentials.
Tenants are resolved per request from the Host subdomain — never from a
client-controlled header.

Upstream: [`github.com/supabase/auth`](https://github.com/supabase/auth) · License: MIT

## How isolation works

```
Browser (dennys.com)
  │
Vercel Next.js ─── supabase.auth.signIn(...)      NEXT_PUBLIC_SUPABASE_AUTH_URL=https://auth-dennys.example.com
  │ HTTPS
Kong (Coolify) ─── routes auth-*.example.com → the ONE auth container
  │
tenant.Middleware ── Host "auth-dennys…" → slug "dennys" → _control._tenants → tenant.Config in ctx
  │
handler: db := a.db.WithContext(ctx)
  │            └── ctx has a tenant → returns that tenant's OWN connection pool
  │                (DSN: …?search_path=dennys_auth,public,extensions)
  │                → every query, transactional or not, is pinned to dennys_auth
  │
  ├── db.Transaction(...)  → hook also runs SET LOCAL search_path (belt & braces / strict mode)
  ├── tokens.SignJWT       → HS256 with the tenant key, kid "tenant:dennys"
  ├── parseJWTClaims       → accepts ONLY that kid/key; other tenants / global key → 403
  ├── mailer               → per-tenant templatemailer (tenant SiteURL + SMTP)
  └── Provider(...)        → tenant OAuth app credentials overlaid
Postgres (shared instance)
  dennys_auth.users        ← only reachable from the dennys pool
  boozegenie_auth.users    ← only reachable from the boozegenie pool
  auth.*                   ← system/background paths only
```

### Three layers, each sufficient on its own

1. **Per-tenant connection pool** (`internal/storage/tenant_pool.go`).
   `Connection.WithContext(ctx)` hands out a pool whose DSN pins
   `search_path` to the tenant schema. pop issues most reads outside
   transactions and has no per-query hook, so scoping *the connection
   itself* is the only way to cover all ~110 upstream call sites without
   touching them. Nothing running on a tenant pool can name another
   tenant's tables.
2. **Transaction hook** (`internal/storage/tenant_hook.go`). Every
   `Connection.Transaction` issues `SET LOCAL search_path` for the tenant
   (or the system schema for `tenant.WithSystem` contexts). `LOCAL` is
   cleared by Postgres at COMMIT/ROLLBACK, so it can never leak across
   pooled connections. In **strict mode** a transaction with neither a
   tenant nor a system marker is refused (`tenant.ErrMissingTenant`).
3. **Key isolation.** Tokens are signed with a per-tenant HS256 key and
   verified only against that tenant's key. Even if a row leaked, a token
   from site A is cryptographically useless at site B.

### Tenant identity

Derived from the first DNS label of `req.Host` (optional `auth-` prefix
stripped), validated against `^[a-z][a-z0-9_-]{2,30}$`, then looked up in
`_control._tenants`. `X-Tenant-*` headers, JWT claims and query strings are
never consulted. Unknown or malformed subdomains get a 404 before any
handler or DB access runs.

## What changed vs upstream

| Path | Change |
|------|--------|
| `internal/tenant/` (new) | `Config`, ctx helpers (`WithConfig`, `FromContext`, `WithSystem`), cached registry `Store`, HTTP `Middleware`, errors |
| `internal/storage/tenant_pool.go` (new) | Lazy per-tenant pools with `search_path` pinned in the DSN |
| `internal/storage/tenant_hook.go` (new) | `SET LOCAL search_path` per transaction; strict mode; explicit `WithTenantTransaction` / `WithTenantSqlDB` |
| `internal/storage/dial.go` | `Connection` remembers its config; `WithContext` routes to tenant pools; `Transaction` calls the hook; `showMaxConns` marked system; exported `SqlDB()` |
| `internal/storage/query.go` (new) | `Query1` / `Exec1` helpers for new strict-mode code |
| `internal/tokens/service.go` + `tenant.go` | `SignJWT` uses the tenant key + kid; `iss` claim per tenant |
| `internal/api/auth.go` | `parseJWTClaims` accepts only the tenant's key under a tenant |
| `internal/api/tenant_overlay.go` (new) | `initMultiTenant`, `tenantConfig(ctx)` overlay (SiteURL, redirect allowlist, SMTP, JWT issuer, Google/GitHub/Discord apps) |
| `internal/api/tenant_mailer.go` (new) | `mailer.Mailer` dispatcher caching one templatemailer per tenant |
| `internal/api/external.go` | `Provider()` and `getExternalRedirectURL()` read the overlaid config |
| `internal/api/middleware.go` | `isValidExternalHost` uses the tenant's own Host as the external host for email links / redirects |
| `internal/api/api.go` | Wires middleware + store when `MultiTenant.Enabled` |
| `internal/conf/configuration.go` | `MultiTenantConfiguration` (`GOTRUE_MULTITENANT_*`) |
| `internal/models/cleanup.go`, `cmd/serve_cmd.go` | Background paths marked `tenant.WithSystem` |
| `db/migrations/001_tenant_control_schema.sql` | `_control._tenants` registry |
| `scripts/lint-tenant-boundary.sh`, `.github/workflows/tenant.yml`, `build.yml` | Lint + tests + GHCR image |

Roughly 1,100 lines added; upstream files are touched in small, local
spots to keep rebases cheap.

## Configuration

| Env | Default | Meaning |
|-----|---------|---------|
| `GOTRUE_MULTITENANT_ENABLED` | `false` | Turn the whole feature on |
| `GOTRUE_MULTITENANT_STRICT` | `false` | Refuse transactions with no tenant and no system marker |
| `GOTRUE_MULTITENANT_CONTROL_DB_URL` | *(main DB)* | Separate Postgres for the registry, if wanted |
| `GOTRUE_MULTITENANT_CACHE_TTL` | `5m` | Registry row cache lifetime |
| `GOTRUE_MULTITENANT_POOL_SIZE` | `4` | Max open connections per tenant pool |

All other `GOTRUE_*` settings act as per-tenant defaults.

## Deployment

1. **Migrate the shared instance as usual** (creates the upstream `auth`
   schema used by system paths):
   ```
   ./auth migrate
   ```
2. **Create the registry**:
   ```
   psql "$DATABASE_URL" -f db/migrations/001_tenant_control_schema.sql
   ```
3. **Provision each tenant's schema with the upstream migrations** — they
   are templated on the namespace, so this yields a complete, correct copy
   (foreign keys, triggers, functions, indexes, and the exact constraint
   names the service's upserts reference):
   ```
   GOTRUE_DB_NAMESPACE=dennys_auth ./auth migrate
   ```
   Never clone tables with `CREATE TABLE … (LIKE … INCLUDING ALL)` — it
   drops foreign keys and renames constraints, which breaks e.g. the
   `mfa_amr_claims` upsert during sign-in.
4. **Register the tenant**:
   ```sql
   insert into _control._tenants
     (slug, schema_name, jwt_secret, jwt_issuer, site_url, redirect_urls,
      smtp_host, smtp_port, smtp_user, smtp_pass, smtp_from)
   values
     ('dennys', 'dennys_auth',
      encode(gen_random_bytes(32), 'base64'),          -- or tenant.GenerateJWTSecret()
      'https://dennysgaragessf.com', 'https://dennysgaragessf.com',
      'https://dennysgaragessf.com/**',
      'smtp.ionos.com', 587, 'office@dennysgaragessf.com', '…', 'office@dennysgaragessf.com');
   ```
5. **Run the image** (built by `.github/workflows/build.yml`):
   ```
   GOTRUE_MULTITENANT_ENABLED=true
   GOTRUE_MULTITENANT_STRICT=true        # after verifying background paths
   ```
   In Coolify, point the Supabase stack's `auth` service at
   `ghcr.io/<you>/auth:multi-tenant` and add a Kong route for
   `auth-*.your-domain.com` → that container.
6. **Per-site Vercel env**:
   ```
   NEXT_PUBLIC_SUPABASE_URL=https://db.your-domain.com
   NEXT_PUBLIC_SUPABASE_AUTH_URL=https://auth-dennys.your-domain.com
   NEXT_PUBLIC_SUPABASE_ANON_KEY=<from Coolify>
   ```
   The JS SDK is unchanged; only the auth URL differs per site.

Config edits take effect within `CACHE_TTL`; bump `updated_at` (the trigger
does it) and per-tenant mailers/overlays rebuild automatically.

## Hardening (recommended in production)

- **Decoy default search_path.** Give the auth DB role a default
  `search_path` that contains no auth tables (e.g. `ALTER USER
  supabase_auth_admin SET search_path = '_no_tenant'`). Tenant pools pin
  their own path, and system paths are pinned to `GOTRUE_DB_NAMESPACE` by
  the transaction hook, so nothing legitimate depends on the default — and
  any query that somehow ran unscoped fails with *relation does not exist*
  instead of silently touching the shared schema. Run `./auth migrate`
  with a URL that sets `?search_path=auth` explicitly.
- **Strict mode on.** `GOTRUE_MULTITENANT_STRICT=true` once the three
  background paths (cleanup, API worker, startup probe — already marked)
  are verified in staging.
- **JWKS.** Tenant tokens are HS256; `/.well-known/jwks.json` still serves
  the global keys. Verify tenant tokens server-side with the tenant secret
  (or through the auth API), not via JWKS.

## Testing

- Unit (no DB): `go test ./internal/tenant/... ./internal/storage/ -run Tenant`
- Lint: `bash scripts/lint-tenant-boundary.sh`
- End to end (needs the migrated test DB plus two migrated tenant schemas —
  the same steps a real deployment uses):
  ```
  for s in t_one_auth t_two_auth; do
    psql postgresql://postgres:root@localhost:5432/postgres \
      -c "create schema if not exists $s authorization supabase_auth_admin"
    # a -c config file overrides process env, so derive a per-tenant file
    sed -e "s|^DB_NAMESPACE=.*|DB_NAMESPACE=\"$s\"|" \
        -e "s|^DATABASE_URL=.*|DATABASE_URL=\"postgres://supabase_auth_admin:root@localhost:5432/postgres?search_path=$s\"|" \
        hack/test.env > /tmp/tenant-$s.env
    go run main.go migrate -c /tmp/tenant-$s.env
  done
  GOTRUE_MULTITENANT_TEST=1 go test ./internal/api -run TestCrossTenantIsolation -v
  ```
  The test skips with instructions if the tenant schemas are missing.
  Proves the same email can sign up on two tenants, each token is rejected
  by the other tenant, unknown subdomains 404, and each schema holds
  exactly its own user.

## Rebasing on upstream

```
git fetch upstream master
git rebase upstream/master          # conflicts, if any, are in the small touched spots above
go test ./...
bash scripts/lint-tenant-boundary.sh
git push --force-with-lease
```

Watch upstream for a native multi-tenancy feature; if it lands, retire the
fork.

## Known limits / follow-ups

- Rate limits are keyed by IP, not by (tenant, IP).
- Postgres auth hooks (`GOTRUE_HOOK_*_URI=pg-functions://…`) name a
  schema-qualified function; they run inside the request transaction (so
  on the tenant pool) but the function itself is shared by all tenants.
- `GetEmailActionLink` (admin generate-link) has no request context and uses
  the global mailer; build tenant links from `tenantConfig(ctx).SiteURL`.
- Only Google, GitHub and Discord have per-tenant OAuth overlays; add more
  in `overlayTenantConfig` as needed.
- Tenant pools live for the process lifetime; idle connections are released
  after `DB.ConnMaxIdleTime`.
