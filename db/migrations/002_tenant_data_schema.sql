-- ============================================================
-- 002: provision each tenant's DATA schema and expose it to PostgREST,
-- automatically, the moment its row is inserted into _control._tenants.
--
-- Run ONCE in Supabase Studio → SQL Editor (Studio runs as supabase_admin,
-- a superuser; that is required because the trigger edits the
-- authenticator role). Safe to re-run.
--
-- What it does on insert (or slug change) of a tenant row:
--   1. create schema <slug with - → _>            (the site's data schema)
--   2. grant it to service_role only               (server-side keys; RLS stays on per table)
--   3. add it to PostgREST's in-database schema list
--        alter role authenticator set pgrst.db_schemas = '...'
--      and NOTIFY pgrst to reload config + schema cache, live, no restart.
-- On delete it removes the schema from the API list (data is NOT dropped).
--
-- Why the database and not the auth image: the auth service connects as
-- supabase_auth_admin, which cannot alter the authenticator role. The
-- in-database setting overrides PGRST_DB_SCHEMAS (PostgREST precedence:
-- file < env < in-database), so the Coolify env var no longer has to be
-- edited per site. Keep it as a sane fallback.
--
-- Sites that need browser (anon/authenticated) access to their schema add
-- those grants themselves in their own schema file; the default here is
-- service_role only.
-- ============================================================

-- Rebuild PostgREST's schema list: whatever is set today, plus every tenant's
-- data schema that exists, minus the ones passed in p_remove. Never drops the
-- Supabase built-ins.
create or replace function _control._sync_api_schemas(p_remove text[] default '{}')
returns text
language plpgsql
security definer
set search_path = ''
as $$
declare
  v_current text;
  v_list    text[];
  v_sch     text;
begin
  select split_part(cfg, '=', 2) into v_current
  from pg_catalog.pg_db_role_setting d
  join pg_catalog.pg_roles r on r.oid = d.setrole
  cross join lateral unnest(d.setconfig) as cfg
  where r.rolname = 'authenticator' and cfg like 'pgrst.db_schemas=%'
  limit 1;

  -- Start from the current in-db list, else the stock Supabase list.
  -- (deduplicated, keeping first-seen order so the list stays readable)
  v_list := array(
    select s.x from (
      select btrim(t.x) as x, min(t.ord) as ord
      from unnest(string_to_array(coalesce(nullif(v_current, ''), 'public,storage,graphql_public'), ',')) with ordinality as t(x, ord)
      where btrim(t.x) <> ''
      group by btrim(t.x)
    ) s order by s.ord
  );
  -- Built-ins always first, then everything else in its existing order.
  v_list := array['public', 'storage', 'graphql_public'] || array(
    select t.x from unnest(v_list) with ordinality as t(x, ord)
    where not t.x = any(array['public', 'storage', 'graphql_public'])
    order by t.ord
  );

  -- Every registered tenant whose data schema exists.
  for v_sch in
    select replace(t.slug, '-', '_')
    from _control._tenants t
    join pg_catalog.pg_namespace n on n.nspname = replace(t.slug, '-', '_')
    order by t.created_at
  loop
    if not v_sch = any(v_list) then v_list := array_append(v_list, v_sch); end if;
  end loop;

  -- Explicit removals (deleted tenants); built-ins are protected.
  v_list := array(
    select t.x from unnest(v_list) with ordinality as t(x, ord)
    where t.x = any(array['public', 'storage', 'graphql_public']) or not t.x = any(p_remove)
    order by t.ord
  );

  execute format('alter role authenticator set pgrst.db_schemas = %L', array_to_string(v_list, ','));
  perform pg_catalog.pg_notify('pgrst', 'reload config');
  perform pg_catalog.pg_notify('pgrst', 'reload schema');
  return array_to_string(v_list, ',');
end;
$$;

-- Create + grant the data schema for one tenant row.
create or replace function _control._provision_data_schema(p_slug text)
returns void
language plpgsql
security definer
set search_path = ''
as $$
declare
  v_sch text := replace(p_slug, '-', '_');
begin
  if v_sch !~ '^[a-z][a-z0-9_]{1,62}$' or v_sch in ('public', 'storage', 'graphql_public', 'auth', 'extensions', '_control') then
    raise exception 'refusing to provision data schema %', v_sch;
  end if;
  execute format('create schema if not exists %I', v_sch);
  execute format('grant usage on schema %I to service_role', v_sch);
  execute format('grant all on all tables    in schema %I to service_role', v_sch);
  execute format('grant all on all sequences in schema %I to service_role', v_sch);
  execute format('grant all on all functions in schema %I to service_role', v_sch);
  execute format('alter default privileges in schema %I grant all on tables    to service_role', v_sch);
  execute format('alter default privileges in schema %I grant all on sequences to service_role', v_sch);
  execute format('alter default privileges in schema %I grant all on functions to service_role', v_sch);
end;
$$;

create or replace function _control._tenants_after_write()
returns trigger
language plpgsql
security definer
set search_path = ''
as $$
begin
  if tg_op = 'DELETE' then
    perform _control._sync_api_schemas(array[replace(old.slug, '-', '_')]);
    return old;
  end if;
  perform _control._provision_data_schema(new.slug);
  if tg_op = 'UPDATE' and old.slug is distinct from new.slug then
    perform _control._sync_api_schemas(array[replace(old.slug, '-', '_')]);
  else
    perform _control._sync_api_schemas();
  end if;
  return new;
end;
$$;

drop trigger if exists _tenants_data_schema on _control._tenants;
create trigger _tenants_data_schema
after insert or delete or update of slug on _control._tenants
for each row execute function _control._tenants_after_write();

-- Only superusers/owners should call these directly.
revoke all on function _control._sync_api_schemas(text[])       from public;
revoke all on function _control._provision_data_schema(text)     from public;
revoke all on function _control._tenants_after_write()           from public;

-- Backfill: provision + expose every tenant already registered.
do $$
declare r record;
begin
  for r in select slug from _control._tenants loop
    perform _control._provision_data_schema(r.slug);
  end loop;
end;
$$;
select _control._sync_api_schemas() as exposed_schemas;
