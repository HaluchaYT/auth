package tenant

import "context"

// ctxKey is unexported so no other package can set or read a tenant on the
// context bypassing this package. The middleware writes it once per request,
// and everything downstream reads it through the exported helpers.
type ctxKey struct{}

var configKey = ctxKey{}

// WithConfig returns a copy of ctx that carries the given tenant config.
// The middleware calls this exactly once per request, right after resolution.
func WithConfig(ctx context.Context, cfg *Config) context.Context {
	if cfg == nil {
		return ctx
	}
	return context.WithValue(ctx, configKey, cfg)
}

// FromContext returns the tenant config attached to ctx, or (nil, false) if
// no tenant has been resolved. The wrapped DB layer treats the false case
// as a hard error — see ErrMissingTenant.
func FromContext(ctx context.Context) (*Config, bool) {
	cfg, ok := ctx.Value(configKey).(*Config)
	return cfg, ok
}

// MustFromContext returns the config or panics. Use only in code paths that
// only run inside a middleware-guarded route; for any DB call, prefer the
// (cfg, ok) form and return ErrMissingTenant on miss.
func MustFromContext(ctx context.Context) *Config {
	cfg, ok := FromContext(ctx)
	if !ok {
		panic("tenant.MustFromContext: no tenant in context — did the middleware run?")
	}
	return cfg
}
