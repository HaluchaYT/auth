package tenant

import (
	"net/http"
	"regexp"
	"strings"
)

// slugPattern is the character-class allowlist for a tenant subdomain. Same
// regex used at ingress and at DB-write time so a value that gets through
// one path can't be smuggled around the other. If you ever consider
// broadening this, remember that the string ends up in a SQL SET LOCAL
// search_path — do not admit anything that pg_identifier doesn't allow.
var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,30}$`)

// Middleware resolves the tenant from the request's Host header and stashes
// the resolved config in context. Downstream handlers pull it with
// tenant.FromContext.
//
// Resolution rules (in order):
//
//  1. Extract the first DNS label of req.Host (e.g. "auth-dennys" from
//     "auth-dennys.example.com").
//  2. Strip the optional "auth-" prefix so admins can name subdomains
//     either "auth-dennys" or just "dennys".
//  3. Validate against slugPattern — reject with 404 if it doesn't match.
//  4. Load from the Store — 404 if the slug isn't registered.
//  5. Attach to context. Forward to next.
//
// By default the middleware never trusts a client-supplied header for tenant
// identity; req.Host is TLS-pinned. Behind a reverse proxy that rewrites the
// upstream Host (Kong without preserve_host, Traefik → Kong chains) enable
// trustForwardedHost so the proxy-set X-Forwarded-Host is used instead. Only
// do this when clients cannot reach the auth container except through that
// proxy, and the proxy overwrites X-Forwarded-Host itself (Kong does).
func Middleware(store *Store, trustForwardedHost bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Liveness/readiness probes are not tenant-scoped. A container
			// health check hits http://127.0.0.1:9999/health, whose Host is
			// not a tenant subdomain, so it must answer regardless of tenant
			// resolution. Without this exemption the probe 404s and the
			// container is reported unhealthy even though tenant traffic
			// (which arrives on auth-<slug>.<domain>) resolves normally.
			if r.URL.Path == "/health" {
				next.ServeHTTP(w, r)
				return
			}
			host := resolveHost(r, trustForwardedHost)
			slug, ok := extractSlug(host)
			if !ok {
				writeUnknownTenant(w)
				return
			}
			cfg, err := store.Load(r.Context(), slug)
			if err != nil {
				writeUnknownTenant(w)
				return
			}
			ctx := WithHost(WithConfig(r.Context(), cfg), host)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// resolveHost picks the host used for tenant resolution. With
// trustForwardedHost the first X-Forwarded-Host value wins when present.
func resolveHost(r *http.Request, trustForwardedHost bool) string {
	if trustForwardedHost {
		if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
			if idx := strings.IndexByte(xfh, ','); idx >= 0 {
				xfh = xfh[:idx]
			}
			return strings.TrimSpace(xfh)
		}
	}
	return r.Host
}

// extractSlug pulls the tenant slug from a Host header. Returns (slug, true)
// on a valid match, ("", false) otherwise. Never returns an unvalidated
// string — callers can hand this straight to Store.Load without additional
// checks.
func extractSlug(host string) (string, bool) {
	// Strip port if present ("localhost:9999" -> "localhost").
	if idx := strings.IndexByte(host, ':'); idx >= 0 {
		host = host[:idx]
	}
	if host == "" {
		return "", false
	}
	first := host
	if idx := strings.IndexByte(host, '.'); idx > 0 {
		first = host[:idx]
	}
	// Convenience: allow "auth-dennys" or "dennys" — the "auth-" prefix
	// exists so operators can namespace DNS records.
	slug := strings.TrimPrefix(first, "auth-")
	if !slugPattern.MatchString(slug) {
		return "", false
	}
	return slug, true
}

func writeUnknownTenant(w http.ResponseWriter) {
	http.Error(w, `{"error":"unknown_tenant"}`, http.StatusNotFound)
}
