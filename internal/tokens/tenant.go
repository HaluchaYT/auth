package tokens

import (
	"context"

	"github.com/supabase/auth/internal/tenant"
)

// tenantIssuer returns the resolved tenant's JWT issuer, or fallback (the
// global config's issuer) when the request has no tenant or the tenant has
// not set one.
func tenantIssuer(ctx context.Context, fallback string) string {
	if tc, ok := tenant.FromContext(ctx); ok && tc.JWTIssuer != "" {
		return tc.JWTIssuer
	}
	return fallback
}
