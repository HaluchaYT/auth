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
// A Config is immutable once placed in context. Changing a tenant's config
// means invalidating the Store cache and letting the next request pick up
// the fresh row from the control table.
type Config struct {
	// Slug is the subdomain-safe identifier used to route requests. Matches
	// the DNS subdomain (auth-<slug>.your-domain.com). Immutable per tenant.
	Slug string

	// Schema is the Postgres schema hosting this tenant's auth tables
	// (users, sessions, refresh_tokens, ...). Every DB call runs under
	// SET LOCAL search_path TO <Schema>.
	Schema string

	// JWTSecret signs JWTs issued for this tenant (base64url-encoded HS256
	// key). Rotating this key invalidates outstanding tokens for the tenant
	// only. See JWTSecretBytes.
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

	// SMTP config for this tenant. Empty SMTPHost means "use the process
	// level defaults from GlobalConfiguration.SMTP".
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	SMTPFrom string

	// External OAuth providers. Enabled=false means "use the global fallback".
	ExternalGoogle  OAuthProvider
	ExternalGitHub  OAuthProvider
	ExternalDiscord OAuthProvider

	// CreatedAt / UpdatedAt for cache invalidation. UpdatedAt is part of the
	// per-tenant derived-config cache key, so bumping it in the control table
	// forces a rebuild of the tenant's overlaid configuration.
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

// JWTSecretBytes returns the raw HS256 signing key for this tenant.
//
// The control table stores the secret base64url-encoded (see
// GenerateJWTSecret). For operator convenience a value that fails to decode
// is used verbatim as a passphrase, matching how upstream treats
// GOTRUE_JWT_SECRET.
func (c *Config) JWTSecretBytes() []byte {
	if c == nil || c.JWTSecret == "" {
		return nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(c.JWTSecret); err == nil && len(b) >= 16 {
		return b
	}
	return []byte(c.JWTSecret)
}

// KeyID is the `kid` header placed on tokens this tenant signs. Prefixed so
// it can never collide with an upstream JWK key id.
func (c *Config) KeyID() string {
	return "tenant:" + c.Slug
}

// HasSMTP reports whether this tenant carries its own SMTP settings.
func (c *Config) HasSMTP() bool {
	return c != nil && c.SMTPHost != ""
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
