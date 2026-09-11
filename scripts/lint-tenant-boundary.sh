#!/usr/bin/env bash
# ============================================================
# Tenant-boundary lint.
#
# Tenant isolation is enforced inside storage.Connection.Transaction()
# (see internal/storage/tenant_hook.go), so every ordinary
# `db.Transaction(...)` call is automatically scoped. The only ways to
# bypass the hook are:
#
#   1. Calling pop's embedded transaction directly:
#        conn.Connection.Transaction(...)
#      Allowed ONLY in internal/storage/dial.go (that is the hook).
#
#   2. Talking to database/sql directly (*sql.DB / *sql.Tx / sqlx) from
#      request-scoped packages. Allowed only in the files listed below.
#
# If this fires, route the call through storage.Connection.Transaction /
# WithTenantTransaction / WithTenantSqlDB, or — for an operator-only
# background path — mark the line with:
#        // multitenant:allow-raw-db <reason>
# ============================================================
set -euo pipefail

fail=0

echo "check 1: direct pop transaction bypass"
if git grep -nE '\.Connection\.Transaction\(' -- 'internal/**/*.go' 'cmd/**/*.go' \
    ':!internal/storage/dial.go' ':!*_test.go' \
    | grep -v 'multitenant:allow-raw-db'; then
  echo "!! pop transaction opened without the tenant hook (see above)"
  fail=1
fi

echo "check 2: raw database/sql in request-scoped packages"
if git grep -nE '\*sql\.(DB|Tx)\b|sqlx\.' -- 'internal/api/**/*.go' 'internal/models/**/*.go' 'internal/tokens/**/*.go' \
    ':!*_test.go' \
    ':!internal/api/tenant_overlay.go' \
    | grep -v 'multitenant:allow-raw-db'; then
  echo "!! raw database/sql usage in a request-scoped package (see above)"
  fail=1
fi

if [[ "$fail" -ne 0 ]]; then
  echo ""
  echo "Tenant boundary lint FAILED."
  exit 1
fi

echo "tenant boundary lint: clean"
