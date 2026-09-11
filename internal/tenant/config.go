// Package tenant provides multi-tenant isolation for the auth service.
//
// The design goal is that GoTrue can serve many isolated auth realms from a
// single process, one Postgres schema per tenant, without cross-tenant leaks.
// Tenant identity is resolved from the request's Host header (subdomain) —
// never from a client-supplied header — and stored in the request context.
// Every DB write and read is then scoped to the tenant's schema via a
// SET LOCAL search_path issued at the start of each transaction.
package tenant

import (
	"crypto/rand"
	"encoding/base64"
	"time"
)

// Config is the resolved configuration for a single tenant.
//
// Fields marked "hot" are read on every request and MUST NOT be mutated
// after the Config is placed in context. Changing a tenant's config means
// invalidating the cache and letting the next request pick up the fresh
// row from the control table.
type Config struct {
	// Slug is the subdomain-safe identifier used to route requests. Matches
	// the DNS subdomain (auth-<slug>.your-domain.com). Immutable per tenant.
	Slug string

	// Schema is the Postgres schema hosting this tenant's auth tables
	// (users, sessions, refresh_tokens, ...). Every DB call runs under
	// SET LOCAL search_path TO <Schema>.
	Schema string

	// JWTSecret signs JWTs issued for this tenant. Rotating this key
	// invalidates outstanding tokens for the tenant only.
	JWTSecret string

	// JWTIssuer becomes the `iss` claim on every JWT this tenant issues.
	// Typically matches SiteURL.
	JWTIssuer string

	// SiteURL is the primary public URL of the tenant's app; used to build
	// callback URLs, email templates, and OAuth redirect defaults.
	SiteURL string

	// RedirectURLs is the allowlist of post-auth redirect targets. Empty
	// means only SiteURL is allowed.
	RedirectURLs []string

	// SMTP config for this tenant. Empty falls back to the process-level
	// defaults from GlobalConfiguration.SMTP.
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	SMTPFrom string

	// External OAuth providers. Empty maps mean "use the global fallback".
	ExternalGoogle   OAuthProvider
	ExternalGitHub   OAuthProvider
	ExternalDiscord  OAuthProvider

	// CreatedAt / UpdatedAt for cache invalidation.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// OAuthProvider holds per-tenant credentials for a specific OAuth provider.
type OAuthProvider struct {
	Enabled      bool
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// GenerateJWTSecret produces a fresh 256-bit HS256 signing key for a new
// tenant. Callers store the returned string in the control table.
func GenerateJWTSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
