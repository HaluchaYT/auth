package tenant

import "context"

// ctxKey is unexported so no other package can set or read a tenant on the
// context bypassing this package. The middleware writes it once per request,
// and everything downstream reads it through the exported helpers.
type ctxKey struct{}

var (
	configKey = ctxKey{}
	systemKey = &struct{ name string }{"tenant.system"}
)

// Strict controls what happens when a DB transaction is opened with no
// tenant in context and no system marker. When true, the transaction is
// refused with ErrMissingTenant. When false (the default, and the behavior
// upstream code expects) the transaction runs against the connection's
// default search_path.
//
// Set from conf.MultiTenantConfiguration.Strict at API construction. Flip
// to true in production once every background path has been marked with
// WithSystem.
var Strict bool

// SystemSchema is the schema operator background paths (cleanup, workers,
// startup probes) are pinned to when they run under WithSystem. Set from
// conf.DBConfiguration.Namespace at API construction; empty means "leave
// the connection's default search_path alone", which is upstream's
// behaviour when multitenant is disabled.
//
// Pinning system paths explicitly lets a hardened deployment point the
// connection's DEFAULT search_path at a decoy schema so that any query
// which somehow runs outside a transaction fails loudly instead of
// silently landing in the shared schema. See MULTITENANT.md → Hardening.
var SystemSchema string

// WithConfig returns a copy of ctx that carries the given tenant config.
// The middleware calls this exactly once per request, right after resolution.
func WithConfig(ctx context.Context, cfg *Config) context.Context {
	if cfg == nil {
		return ctx
	}
	return context.WithValue(ctx, configKey, cfg)
}

// FromContext returns the tenant config attached to ctx, or (nil, false) if
// no tenant has been resolved. The storage layer treats the false case as a
// hard error in Strict mode — see ErrMissingTenant.
func FromContext(ctx context.Context) (*Config, bool) {
	if ctx == nil {
		return nil, false
	}
	cfg, ok := ctx.Value(configKey).(*Config)
	return cfg, ok && cfg != nil
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

// WithSystem marks ctx as belonging to an operator-controlled background
// path (cleanup workers, migrations, connection-limit probes, the API
// worker). Transactions opened under a system context run against the
// connection's default search_path even when Strict is on.
//
// Never derive a request context from a system context: a request that
// inherits the system marker would bypass tenant isolation in Strict mode.
func WithSystem(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemKey, true)
}

// IsSystem reports whether ctx carries the system marker.
func IsSystem(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(systemKey).(bool)
	return v
}
