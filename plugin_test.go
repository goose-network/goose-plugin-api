package plugin

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestDefaultProviderRegistry(t *testing.T) {
	RegisterDefaultProvider(DefaultProvider{Provider: "zero-config"})

	// An omitted ID defaults to the provider name; Config stays nil (the
	// zero-config form).
	var dc *DefaultProvider
	for _, d := range LookupDefaultProviders() {
		if d.Provider == "zero-config" {
			dc = &d
			break
		}
	}
	if dc == nil {
		t.Fatal("declared default not returned by LookupDefaultProviders")
	}
	if dc.ID != "zero-config" {
		t.Fatalf("ID = %q, want it defaulted to the provider name %q", dc.ID, "zero-config")
	}
	if dc.Config != nil {
		t.Fatalf("Config = %v, want nil (zero-config form)", dc.Config)
	}
	if _, ok := LookupDefaultProvider(); !ok {
		t.Fatal("legacy singular LookupDefaultProvider should report a declared default")
	}

	// Declarations accumulate: a second, different plugin does NOT panic —
	// the engine seeds every zero-config plugin linked into the binary.
	RegisterDefaultProvider(DefaultProvider{Provider: "other"})
	if len(LookupDefaultProviders()) < 2 {
		t.Fatal("two distinct declarations should accumulate")
	}

	// The same provider name declared twice panics: a plugin registering
	// itself twice is a bug.
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate RegisterDefaultProvider should panic")
		}
	}()
	RegisterDefaultProvider(DefaultProvider{Provider: "zero-config"})
}

func TestRegisterDefaultProviderEmptyProviderPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("RegisterDefaultProvider with empty Provider should panic")
		}
	}()
	RegisterDefaultProvider(DefaultProvider{ID: "x"})
}

func TestAsStackDialer(t *testing.T) {
	if d := AsStackDialer(nil); d != nil {
		t.Fatalf("AsStackDialer(nil) = %v, want nil", d)
	}
	if d := AsStackDialer(map[string]any{}); d != nil {
		t.Fatalf("AsStackDialer({}) = %v, want nil", d)
	}

	dialer := stubStackDialer{}
	cfg := map[string]any{StackDialerKey: dialer}
	got := AsStackDialer(cfg)
	if got != StackDialer(dialer) {
		t.Fatalf("AsStackDialer did not extract the injected dialer")
	}
}

// stubStackDialer satisfies StackDialer; it never dials.
type stubStackDialer struct{}

func (stubStackDialer) Dial(_ context.Context, _, _ string) (net.Conn, error) {
	return nil, errors.New("not implemented")
}
