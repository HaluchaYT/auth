package api

import (
	"net/mail"
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/gobwas/glob"
	"github.com/sirupsen/logrus"

	"github.com/supabase/auth/internal/conf"
	"github.com/supabase/auth/internal/storage"
	"github.com/supabase/auth/internal/tenant"
)

// tenantOverlayCache memoises one derived *conf.GlobalConfiguration per
// tenant. Entries are rebuilt when the tenant's UpdatedAt changes, i.e. when
// its registry row was edited and the Store handed us a fresh Config.
type tenantOverlayCache struct {
	mu    sync.RWMutex
	items map[string]tenantOverlayEntry
}

type tenantOverlayEntry struct {
	updatedAt time.Time
	cfg       *conf.GlobalConfiguration
}

// initMultiTenant wires the tenant registry, the strict-mode switch and the
// per-tenant mailer. Called from NewAPIWithVersion when
// config.MultiTenant.Enabled is set.
func (a *API) initMultiTenant(cfg *conf.GlobalConfiguration, db *storage.Connection) {
	log := logrus.WithField("component", "multitenant")

	sqldb := db.SqlDB()
	if url := cfg.MultiTenant.ControlDBURL; url != "" {
		// "pgx" is registered by pop's postgres dialect, which is always
		// linked in — no extra driver import needed.
		d, err := sql.Open("pgx", url)
		if err != nil {
			log.WithError(err).Error("unable to open control DB; falling back to main connection")
		} else {
			sqldb = d
		}
	}
	if sqldb == nil {
		log.Error("no *sql.DB available for the tenant registry — multitenant stays disabled")
		return
	}

	ttl := cfg.MultiTenant.CacheTTL
	if ttl <= 0 {
		ttl = tenant.DefaultCacheTTL
	}

	a.tenantStore = tenant.NewStore(sqldb, tenant.WithCacheTTL(ttl))
	a.tenantOverlays.items = make(map[string]tenantOverlayEntry)
	tenant.Strict = cfg.MultiTenant.Strict
	// Background/system paths are pinned to the upstream namespace (default
	// "auth") so they never rely on the connection's default search_path.
	tenant.SystemSchema = cfg.DB.Namespace

	// Wrap whatever mailer was configured (WithMailer option or the default)
	// so each tenant's mail carries its own SiteURL and SMTP identity.
	a.mailer = newTenantMailer(a, a.mailer)

	log.WithFields(logrus.Fields{
		"strict":        tenant.Strict,
		"system_schema": tenant.SystemSchema,
		"cache_ttl":     ttl.String(),
	}).Info("multitenant mode enabled")
}

// tenantConfig returns the effective configuration for a request: the global
// config with the resolved tenant's SiteURL, redirect allowlist, SMTP, JWT
// issuer and OAuth app settings overlaid. With no tenant in ctx it returns
// a.config itself, so every upstream call site keeps its exact behaviour.
func (a *API) tenantConfig(ctx context.Context) *conf.GlobalConfiguration {
	tc, ok := tenant.FromContext(ctx)
	if !ok {
		return a.config
	}

	a.tenantOverlays.mu.RLock()
	e, hit := a.tenantOverlays.items[tc.Slug]
	a.tenantOverlays.mu.RUnlock()
	if hit && e.updatedAt.Equal(tc.UpdatedAt) {
		return e.cfg
	}

	cfg := overlayTenantConfig(a.config, tc)

	a.tenantOverlays.mu.Lock()
	if a.tenantOverlays.items == nil {
		a.tenantOverlays.items = make(map[string]tenantOverlayEntry)
	}
	a.tenantOverlays.items[tc.Slug] = tenantOverlayEntry{updatedAt: tc.UpdatedAt, cfg: cfg}
	a.tenantOverlays.mu.Unlock()
	return cfg
}

// overlayTenantConfig derives a tenant-specific configuration from base.
// The copy is shallow: nested structs are copied by value, and the maps or
// slices we touch are replaced, never mutated, so base is never modified.
func overlayTenantConfig(base *conf.GlobalConfiguration, tc *tenant.Config) *conf.GlobalConfiguration {
	cfg := *base

	if tc.SiteURL != "" {
		cfg.SiteURL = tc.SiteURL
	}

	if len(tc.RedirectURLs) > 0 {
		cfg.URIAllowList = append([]string(nil), tc.RedirectURLs...)
		cfg.URIAllowListMap = make(map[string]glob.Glob, len(cfg.URIAllowList))
		for _, uri := range cfg.URIAllowList {
			g, err := glob.Compile(uri, '.', '/')
			if err != nil {
				logrus.WithError(err).WithField("tenant", tc.Slug).Warnf("ignoring invalid redirect allowlist entry %q", uri)
				continue
			}
			cfg.URIAllowListMap[uri] = g
		}
	}

	if tc.HasSMTP() {
		cfg.SMTP.Host = tc.SMTPHost
		if tc.SMTPPort > 0 {
			cfg.SMTP.Port = tc.SMTPPort
		}
		cfg.SMTP.User = tc.SMTPUser
		cfg.SMTP.Pass = tc.SMTPPass
		if tc.SMTPFrom != "" {
			// smtp_from may carry a display name: "Denny's Garage <office@example.com>".
			// Split it so the From header shows the name (a real deliverability
			// signal) and the address stays what the SMTP account authorises.
			if addr, perr := mail.ParseAddress(tc.SMTPFrom); perr == nil {
				cfg.SMTP.AdminEmail = addr.Address
				if addr.Name != "" {
					cfg.SMTP.SenderName = addr.Name
				}
			} else {
				cfg.SMTP.AdminEmail = tc.SMTPFrom
			}
		}
		// Recompute the cached from-address / normalised headers for the
		// new identity. Validate never fails for SMTP.
		_ = cfg.SMTP.Validate()
	}

	if tc.JWTIssuer != "" {
		cfg.JWT.Issuer = tc.JWTIssuer
	}

	overlayProvider(&cfg.External.Google, tc.ExternalGoogle)
	overlayProvider(&cfg.External.Github, tc.ExternalGitHub)
	overlayProvider(&cfg.External.Discord, tc.ExternalDiscord)

	return &cfg
}

// overlayProvider swaps in a tenant's own OAuth app credentials. A tenant
// that has not configured the provider keeps the global app, if any.
func overlayProvider(dst *conf.OAuthProviderConfiguration, src tenant.OAuthProvider) {
	if !src.Enabled {
		return
	}
	dst.Enabled = true
	dst.ClientID = []string{src.ClientID}
	dst.Secret = src.ClientSecret
	if src.RedirectURL != "" {
		dst.RedirectURI = src.RedirectURL
	}
}
