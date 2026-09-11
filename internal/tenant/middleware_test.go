package tenant

import (
	"net/http/httptest"
	"testing"
)

func TestResolveHost(t *testing.T) {
	r := httptest.NewRequest("GET", "http://auth-dennys.example.com/token", nil)
	r.Host = "auth-dennys.example.com"
	r.Header.Set("X-Forwarded-Host", "auth-boozegenie.example.com, kong.internal")

	if got := resolveHost(r, false); got != "auth-dennys.example.com" {
		t.Fatalf("untrusted: got %q", got)
	}
	if got := resolveHost(r, true); got != "auth-boozegenie.example.com" {
		t.Fatalf("trusted: got %q", got)
	}
	r.Header.Del("X-Forwarded-Host")
	if got := resolveHost(r, true); got != "auth-dennys.example.com" {
		t.Fatalf("trusted without header: got %q", got)
	}
}

func TestExtractSlug(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		wantSlug string
		wantOK   bool
	}{
		{"bare slug", "dennys.example.com", "dennys", true},
		{"auth prefix", "auth-dennys.example.com", "dennys", true},
		{"with port", "dennys.example.com:8443", "dennys", true},
		{"just slug no dot", "dennys", "dennys", true},
		{"uppercase rejected", "Dennys.example.com", "", false},
		{"digit start rejected", "1dennys.example.com", "", false},
		{"too short rejected", "de.example.com", "", false},
		{"empty host", "", "", false},
		{"symbol rejected", "boo$mgenie.example.com", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slug, ok := extractSlug(tc.host)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if slug != tc.wantSlug {
				t.Fatalf("slug = %q, want %q", slug, tc.wantSlug)
			}
		})
	}
}
