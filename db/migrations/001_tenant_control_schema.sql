-- ============================================================
-- Multi-tenant control schema.
--
-- This migration lives OUTSIDE any tenant's own auth schema. It holds
-- the registry of tenants and their configuration. The auth service reads
-- this table at request time (via internal/tenant.Store) to resolve
-- tenants from their subdomain.
--
-- Idempotent — safe to re-run.
-- ============================================================

create schema if not exists _control;

create table if not exists _control._tenants (
  slug                 text primary key,
    -- subdomain-safe identifier, e.g. 'dennys' from auth-dennys.example.com
  schema_name          text not null,
    -- Postgres schema hosting this tenant's auth tables, e.g. 'dennys_auth'
  jwt_secret           text not null,
    -- per-tenant HS256 signing key (base64url); rotate to log out all users
  jwt_issuer           text not null,
    -- becomes the iss claim on tokens; usually matches site_url
  site_url             text not null,
  redirect_urls        text default '',
    -- comma-separated allowlist of post-auth redirect targets

  smtp_host            text not null default '',
  smtp_port            integer not null default 587,
  smtp_user            text not null default '',
  smtp_pass            text not null default '',
  smtp_from            text not null default '',

  created_at           timestamptz not null default now(),
  updated_at           timestamptz not null default now(),

  constraint tenants_schema_name_format check (schema_name ~ '^[a-z_][a-z0-9_]{0,62}$'),
  constraint tenants_slug_format        check (slug ~ '^[a-z][a-z0-9_-]{2,30}$')
);

create index if not exists _tenants_schema_idx on _control._tenants (schema_name);

-- Auto-update updated_at
create or replace function _control._set_updated_at()
returns trigger language plpgsql as $$
begin
  new.updated_at := now();
  return new;
end;
$$;

drop trigger if exists _tenants_updated_at on _control._tenants;
create trigger _tenants_updated_at
before update on _control._tenants
for each row execute function _control._set_updated_at();

-- ============================================================
-- Bootstrap helper: create a new tenant's auth schema and copy the
-- upstream GoTrue tables into it. Call once per tenant AFTER GoTrue's
-- upstream migrations have created the reference `auth` schema.
--
-- Usage:
--   select _control.provision_tenant_schema('dennys_auth');
-- ============================================================
create or replace function _control.provision_tenant_schema(target_schema text)
returns void language plpgsql as $$
declare
  tbl record;
begin
  if target_schema !~ '^[a-z_][a-z0-9_]{0,62}$' then
    raise exception 'invalid schema name: %', target_schema;
  end if;

  execute format('create schema if not exists %I', target_schema);

  for tbl in
    select tablename from pg_tables where schemaname = 'auth'
  loop
    execute format(
      'create table if not exists %I.%I (like auth.%I including all)',
      target_schema, tbl.tablename, tbl.tablename
    );
  end loop;
end;
$$;
