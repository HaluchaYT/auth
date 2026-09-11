package api

import (
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/supabase/auth/internal/mailer"
	"github.com/supabase/auth/internal/mailer/templatemailer"
	"github.com/supabase/auth/internal/models"
	"github.com/supabase/auth/internal/tenant"
)

// tenantMailer routes every outgoing email to a per-tenant templatemailer
// built from that tenant's overlaid configuration (SiteURL, SMTP identity,
// redirect allowlist). Confirmation and magic-link emails therefore carry
// the right links and come from the right sender for the site the user is
// signing in to. Requests with no resolved tenant use the process-wide
// mailer exactly as upstream does.
//
// One templatemailer is cached per tenant and rebuilt when the tenant's
// UpdatedAt changes (i.e. the registry row was edited).
type tenantMailer struct {
	api      *API
	fallback mailer.Mailer
	tc       *templatemailer.Cache

	mu     sync.RWMutex
	bySlug map[string]tenantMailerEntry
}

type tenantMailerEntry struct {
	updatedAt time.Time
	m         mailer.Mailer
}

func newTenantMailer(api *API, fallback mailer.Mailer) *tenantMailer {
	return &tenantMailer{
		api:      api,
		fallback: fallback,
		tc:       templatemailer.NewCache(),
		bySlug:   make(map[string]tenantMailerEntry),
	}
}

// pick returns the mailer for the request's tenant, or the fallback.
func (m *tenantMailer) pick(r *http.Request) mailer.Mailer {
	if r == nil {
		return m.fallback
	}
	tc, ok := tenant.FromContext(r.Context())
	if !ok {
		return m.fallback
	}

	m.mu.RLock()
	e, hit := m.bySlug[tc.Slug]
	m.mu.RUnlock()
	if hit && e.updatedAt.Equal(tc.UpdatedAt) {
		return e.m
	}

	built := templatemailer.FromConfig(m.api.tenantConfig(r.Context()), m.tc)

	m.mu.Lock()
	m.bySlug[tc.Slug] = tenantMailerEntry{updatedAt: tc.UpdatedAt, m: built}
	m.mu.Unlock()
	return built
}

func (m *tenantMailer) InviteMail(r *http.Request, user *models.User, otp, referrerURL string, externalURL *url.URL) error {
	return m.pick(r).InviteMail(r, user, otp, referrerURL, externalURL)
}

func (m *tenantMailer) ConfirmationMail(r *http.Request, user *models.User, otp, referrerURL string, externalURL *url.URL) error {
	return m.pick(r).ConfirmationMail(r, user, otp, referrerURL, externalURL)
}

func (m *tenantMailer) RecoveryMail(r *http.Request, user *models.User, otp, referrerURL string, externalURL *url.URL) error {
	return m.pick(r).RecoveryMail(r, user, otp, referrerURL, externalURL)
}

func (m *tenantMailer) MagicLinkMail(r *http.Request, user *models.User, otp, referrerURL string, externalURL *url.URL) error {
	return m.pick(r).MagicLinkMail(r, user, otp, referrerURL, externalURL)
}

func (m *tenantMailer) EmailChangeMail(r *http.Request, user *models.User, otpNew, otpCurrent, referrerURL string, externalURL *url.URL) error {
	return m.pick(r).EmailChangeMail(r, user, otpNew, otpCurrent, referrerURL, externalURL)
}

func (m *tenantMailer) ReauthenticateMail(r *http.Request, user *models.User, otp string) error {
	return m.pick(r).ReauthenticateMail(r, user, otp)
}

// GetEmailActionLink has no request in its signature, so there is no tenant
// to resolve; it uses the fallback mailer. Admin generate-link callers that
// need tenant-scoped links should build them from tenantConfig(ctx).SiteURL.
func (m *tenantMailer) GetEmailActionLink(user *models.User, actionType, referrerURL string, externalURL *url.URL) (string, error) {
	return m.fallback.GetEmailActionLink(user, actionType, referrerURL, externalURL)
}

func (m *tenantMailer) PasswordChangedNotificationMail(r *http.Request, user *models.User) error {
	return m.pick(r).PasswordChangedNotificationMail(r, user)
}

func (m *tenantMailer) EmailChangedNotificationMail(r *http.Request, user *models.User, oldEmail string) error {
	return m.pick(r).EmailChangedNotificationMail(r, user, oldEmail)
}

func (m *tenantMailer) PhoneChangedNotificationMail(r *http.Request, user *models.User, oldPhone string) error {
	return m.pick(r).PhoneChangedNotificationMail(r, user, oldPhone)
}

func (m *tenantMailer) IdentityLinkedNotificationMail(r *http.Request, user *models.User, provider string) error {
	return m.pick(r).IdentityLinkedNotificationMail(r, user, provider)
}

func (m *tenantMailer) IdentityUnlinkedNotificationMail(r *http.Request, user *models.User, provider, recipientEmail string) error {
	return m.pick(r).IdentityUnlinkedNotificationMail(r, user, provider, recipientEmail)
}

func (m *tenantMailer) MFAFactorEnrolledNotificationMail(r *http.Request, user *models.User, factorType string) error {
	return m.pick(r).MFAFactorEnrolledNotificationMail(r, user, factorType)
}

func (m *tenantMailer) MFAFactorUnenrolledNotificationMail(r *http.Request, user *models.User, factorType string) error {
	return m.pick(r).MFAFactorUnenrolledNotificationMail(r, user, factorType)
}

// compile-time interface check
var _ mailer.Mailer = (*tenantMailer)(nil)
