# Multi-tenant fork of `supabase/auth`

This fork adds **native tenant isolation** to Supabase's auth service. A
single container serves many isolated auth realms, each with its own users
table, JWT signing key, SMTP config, and OAuth apps. Tenants are resolved
per request from the subdomain of the incoming Host header — never from a
client-controlled header.

Upstream: [`github.com/supabase/auth`](https://github.com/supabase/auth)

## Why

Stock GoTrue binds to one `auth` schema and one JWT signing key. To run
multiple isolated auth realms on one Postgres, you either:

1. Deploy N containers (one per tenant) — ~50 MB RAM each, wasteful at scale.
2. Share one auth realm across sites — email uniqueness is global, tokens
   cross site boundaries.

Neither is great past ~10 tenants. This fork adds a middleware that
resolves the tenant per request and a wrapped-DB pattern that guarantees
every query runs under the tenant's Postgres schema — impossible to leak
cross-tenant by construction.

## Architecture

```
Browser (dennys.com)
  │
Vercel Next.js  ─── supabase.auth.signIn({...})
  │ HTTPS
Kong gateway     ─── matches Host: auth-dennys.your-domain.com
  │
Forked auth (single container)
  │
tenant.Middleware  ─── req.Host → slug → tenant.Config → context.Context
  │
storage.Connection.WithTenantTransaction(ctx, ...)
  │                ─── BEGIN
  │                ─── SET LOCAL search_path TO dennys_auth
  │                ─── model DB ops
  │                ─── COMMIT   (search_path auto-resets)
Postgres (shared)
  dennys_auth.users        ← hits ONLY this schema
  boozegenie_auth.users    ← untouched
```

### Two invariants the design enforces

1. **No tenant in context → no query runs.** `WithTenantTransaction`
   returns `tenant.ErrMissingTenant` before opening any DB resource.
2. **Every DB call is scoped by `SET LOCAL`.** `LOCAL` auto-resets at
   COMMIT / ROLLBACK, so pooled connections can never leak the previous
   request's schema.

### Tenant resolution

Tenant identity comes from the first DNS label of `req.Host`. That value
is TLS-pinned and Kong-verified, so a client can't spoof it. An optional
`auth-` prefix (e.g. `auth-dennys.example.com`) lets ops keep DNS namespaces
tidy — the middleware strips it.

Client-controlled sources (headers, JWT claims, query strings) are
**never** consulted for tenant identity.

## Package layout

| Path | Role |
|------|------|
| `internal/tenant/config.go` | `Config` struct — per-tenant JWT / SMTP / schema |
| `internal/tenant/context.go` | Helpers for stashing / reading `*Config` on ctx |
| `internal/tenant/store.go` | Cached loader from `_control._tenants` |
| `internal/tenant/middleware.go` | HTTP middleware: subdomain → slug → Config |
| `internal/tenant/errors.go` | Sentinel errors (`ErrMissingTenant`, `ErrUnknownTenant`) |
| `internal/storage/tenant_hook.go` | `WithTenantTransaction` / `WithTenantSqlDB` — the tenant-scoped DB wrappers |
| `db/migrations/001_tenant_control_schema.sql` | `_control` schema + `_tenants` table + `provision_tenant_schema()` |
| `scripts/lint-tenant-boundary.sh` | CI grep backstop — fails if a model uses a raw DB primitive |
| `.github/workflows/build.yml` | Builds and pushes the image to GHCR on every push |
| `.github/workflows/test.yml` | Runs unit + integration tests + the boundary lint |

## Deployment

1. **Provision the control schema** in your shared Postgres:
   ```
   psql "$DATABASE_URL" -f db/migrations/001_tenant_control_schema.sql
   ```

2. **Register a tenant** in `_control._tenants`. Use `provision_tenant_schema`
   to seed the tenant's schema with copies of the upstream auth tables:
   ```sql
   select _control.provision_tenant_schema('dennys_auth');

   insert into _control._tenants (
     slug, schema_name, jwt_secret, jwt_issuer, site_url, redirect_urls
   ) values (
     'dennys',
     'dennys_auth',
     encode(gen_random_bytes(32), 'base64'),
     'https://dennysgaragessf.com',
     'https://dennysgaragessf.com',
     'https://dennysgaragessf.com/auth/callback'
   );
   ```

3. **Point Coolify's auth container at your image** — `ghcr.io/<your-org>/auth:multi-tenant`
   is built and pushed on every merge to `feat/tenant-schema-routing` by
   `.github/workflows/build.yml`.

4. **DNS: point `auth-<slug>.your-domain.com`** at your VPS. Kong forwards
   to the auth container; the middleware reads the subdomain and resolves
   the tenant.

5. **Per-site Vercel env**:
   ```
   NEXT_PUBLIC_SUPABASE_URL=https://db.your-domain.com
   NEXT_PUBLIC_SUPABASE_AUTH_URL=https://auth-dennys.your-domain.com
   NEXT_PUBLIC_SUPABASE_ANON_KEY=<from Coolify>
   ```

The Supabase JS SDK stays unchanged; only the auth URL differs per site.

## Rebasing on upstream

Monthly workflow:

```
git remote add upstream https://github.com/supabase/auth.git
git fetch upstream master
git rebase upstream/master
# resolve conflicts, usually in internal/models/*.go
go test -race ./...
bash scripts/lint-tenant-boundary.sh
git push --force-with-lease
```

## Status

- [x] `internal/tenant/` package — config, context, store, middleware, errors
- [x] Storage-level `WithTenantTransaction` hook
- [x] `_control._tenants` schema + `provision_tenant_schema()` helper
- [x] GitHub Actions build workflow → GHCR
- [x] Test workflow with boundary lint
- [x] Unit tests for context + middleware
- [ ] Thread ctx through `internal/models/*.go` (Day 3)
- [ ] Per-tenant JWT signing (Day 4)
- [ ] Per-tenant SMTP (Day 4)
- [ ] Per-tenant OAuth (Day 4)
- [ ] Integration `TestCrossTenantIsolation` in `internal/api/` (Day 5)
- [ ] Coolify integration test (Day 6)
