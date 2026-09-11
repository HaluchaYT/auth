package storage

import "testing"

func TestTenantIsSafeIdentifier(t *testing.T) {
	ok := []string{"dennys_auth", "a", "auth", "boozegenie_auth", "t1"}
	for _, s := range ok {
		if !isSafeIdentifier(s) {
			t.Errorf("expected %q to be safe", s)
		}
	}
	bad := []string{"", "Dennys", "1abc", "a-b", "a b", "a;drop", `a"b`, "public.users",
		"x_________________________________________________________________"} // >63 chars
	for _, s := range bad {
		if isSafeIdentifier(s) {
			t.Errorf("expected %q to be rejected", s)
		}
	}
}

func TestTenantPqQuoteIdent(t *testing.T) {
	if got := pqQuoteIdent("dennys_auth"); got != `"dennys_auth"` {
		t.Fatalf("got %s", got)
	}
	if got := pqQuoteIdent(`we"ird`); got != `"we""ird"` {
		t.Fatalf("got %s", got)
	}
}

func TestTenantWithSearchPath(t *testing.T) {
	got, err := withSearchPath("postgres://u:p@localhost:5432/postgres?sslmode=disable", "dennys_auth")
	if err != nil {
		t.Fatal(err)
	}
	want := "postgres://u:p@localhost:5432/postgres?search_path=dennys_auth%2Cpublic%2Cextensions&sslmode=disable"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// an existing search_path on the base DSN is replaced, never appended
	got, err = withSearchPath("postgres://u:p@h/db?search_path=auth", "t1_auth")
	if err != nil {
		t.Fatal(err)
	}
	if want := "postgres://u:p@h/db?search_path=t1_auth%2Cpublic%2Cextensions"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if _, err := withSearchPath("postgres://u:p@h/db", "Bad-Schema"); err == nil {
		t.Fatal("expected invalid schema to be rejected")
	}
}

func TestTenantSearchPathSQL(t *testing.T) {
	got := tenantSearchPathSQL("dennys_auth")
	want := `SET LOCAL search_path TO "dennys_auth", public, extensions`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
