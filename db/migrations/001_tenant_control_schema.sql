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

-- ------------------------------------------------------------
-- Dashboard-friendly defaults: registering a tenant from Supabase
-- Studio's Table Editor ("Insert row") only needs `slug` and `site_url`.
--   jwt_secret   → generated 256-bit key (base64url)
--   schema_name  → '<slug with dashes→underscores>_auth'
--   jwt_issuer   → site_url
-- ------------------------------------------------------------
create extension if not exists pgcrypto;

alter table _control._tenants
  alter column jwt_secret set default translate(encode(gen_random_bytes(32), 'base64'), '+/=', '-_'),
  alter column schema_name drop not null,
  alter column jwt_issuer  drop not null;

create or replace function _control._tenants_defaults()
returns trigger language plpgsql set search_path = '' as $$
begin
  if new.schema_name is null or new.schema_name = '' then
    new.schema_name := replace(new.slug, '-', '_') || '_auth';
  end if;
  if new.jwt_issuer is null or new.jwt_issuer = '' then
    new.jwt_issuer := new.site_url;
  end if;
  return new;
end;
$$;

drop trigger if exists _tenants_defaults on _control._tenants;
create trigger _tenants_defaults
before insert on _control._tenants
for each row execute function _control._tenants_defaults();

-- Auto-update updated_at
create or replace function _control._set_updated_at()
returns trigger language plpgsql set search_path = '' as $$
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
-- Provisioning a tenant's auth schema.
--
-- Always run the upstream migrations against the tenant namespace. Every
-- migration file is templated on {{ index .Options "Namespace" }}, so this
-- produces a complete, correct schema — tables, foreign keys, triggers,
-- functions, indexes AND the exact constraint names the service relies on
-- (e.g. ON CONFLICT ON CONSTRAINT mfa_amr_claims_session_id_authentication_method_pkey):
--
--   GOTRUE_DB_NAMESPACE=dennys_auth ./auth migrate
--
-- Do NOT clone tables with CREATE TABLE ... (LIKE auth.x INCLUDING ALL):
-- LIKE drops foreign keys and gives copied constraints generated names,
-- which breaks upserts inside the service.
-- ============================================================

-- ============================================================
-- Privileges for the auth service role (Supabase stacks run auth as
-- supabase_auth_admin). It must read the registry and, for
-- GOTRUE_MULTITENANT_AUTO_PROVISION, create tenant schemas.
-- No-op when the role does not exist (bare Postgres / CI).
-- ============================================================
do $$
begin
  if exists (select 1 from pg_roles where rolname = 'supabase_auth_admin') then
    grant usage on schema _control to supabase_auth_admin;
    grant select on _control._tenants to supabase_auth_admin;
    execute format('grant create on database %I to supabase_auth_admin', current_database());
  end if;
end;
$$;
