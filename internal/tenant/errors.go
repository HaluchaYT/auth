package tenant

import "errors"

// ErrMissingTenant is returned by the wrapped DB layer when a query is
// attempted without a resolved tenant in context. This is a structural
// guarantee, not a runtime warning: every DB call must go through a
// tenant-aware transaction.
var ErrMissingTenant = errors.New("tenant: no tenant in context")

// ErrUnknownTenant is returned by the middleware when a request arrives
// for a subdomain that has no row in _control._tenants.
var ErrUnknownTenant = errors.New("tenant: unknown tenant")

// ErrInvalidSlug is returned when a subdomain fails the character-class
// check. Never let an invalid slug reach Postgres.
var ErrInvalidSlug = errors.New("tenant: invalid slug format")
