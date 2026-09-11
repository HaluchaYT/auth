package tenant

import (
	"context"
	"testing"
)

func TestWithConfigFromContext(t *testing.T) {
	ctx := context.Background()

	if _, ok := FromContext(ctx); ok {
		t.Fatal("empty context should have no tenant")
	}

	cfg := &Config{Slug: "dennys", Schema: "dennys_auth"}
	ctx = WithConfig(ctx, cfg)

	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("expected tenant in context")
	}
	if got.Slug != "dennys" || got.Schema != "dennys_auth" {
		t.Fatalf("unexpected cfg: %+v", got)
	}
}

func TestWithConfigNil(t *testing.T) {
	ctx := WithConfig(context.Background(), nil)
	if _, ok := FromContext(ctx); ok {
		t.Fatal("nil config should not populate context")
	}
}

func TestMustFromContextPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustFromContext should panic when tenant missing")
		}
	}()
	_ = MustFromContext(context.Background())
}
