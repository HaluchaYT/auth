#!/usr/bin/env bash
# ============================================================
# Grep backstop for the tenant boundary invariant.
#
# The wrapped-DB pattern (see internal/storage/tenant_hook.go) makes it
# structurally impossible to run a query without a tenant in context —
# BUT that only holds if every model call goes through
# WithTenantTransaction / WithTenantSqlDB.
#
# This script fails CI if it detects a model file directly using the
# naked storage.Connection.Transaction() or a raw *sql.DB. When you see
# it fire, either:
#
#   1. Convert the call site to WithTenantTransaction (usually correct)
#   2. If the code path is genuinely tenant-independent (bootstrap,
#      migrations, control table access), add a nolint comment:
#        // multitenant:allow-raw-db  <reason>
# ============================================================
set -euo pipefail

# Files under internal/models are always request-scoped and must use
# the tenant-aware helpers. Anything using the naked Transaction() or
# raw *sql.DB in this directory is a red flag.
if git grep -nE '\.Transaction\(|\*sql\.DB' -- 'internal/models/*.go' \
    ':!*_test.go' \
    | grep -v 'multitenant:allow-raw-db'; then
  echo ""
  echo "!! Tenant boundary lint failed."
  echo "!! One or more model call sites is using a raw DB primitive instead"
  echo "!! of WithTenantTransaction. Route it through the tenant hook or"
  echo "!! add a 'multitenant:allow-raw-db' comment with a reason."
  exit 1
fi

echo "tenant boundary lint: clean"
