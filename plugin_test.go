package plugin

import "testing"

func TestDefaultProviderRegistry(t *testing.T) {
	if _, ok := LookupDefaultProvider(); ok {
		t.Fatal("no default declared yet, LookupDefaultProvider should report false")
	}

	RegisterDefaultProvider(DefaultProvider{Provider: "zero-config"})

	got, ok := LookupDefaultProvider()
	if !ok {
		t.Fatal("default declared, LookupDefaultProvider should report it")
	}
	if got.Provider != "zero-config" {
		t.Fatalf("Provider = %q, want %q", got.Provider, "zero-config")
	}
	// An omitted ID defaults to the provider name.
	if got.ID != "zero-config" {
		t.Fatalf("ID = %q, want it defaulted to the provider name %q", got.ID, "zero-config")
	}
	if got.Config != nil {
		t.Fatalf("Config = %v, want nil (zero-config form)", got.Config)
	}

	// A second declaration panics: the default slot is singular, and a
	// binary linking two default-declaring plugins should fail at startup
	// instead of picking an init-order winner.
	defer func() {
		if recover() == nil {
			t.Fatal("second RegisterDefaultProvider should panic")
		}
	}()
	RegisterDefaultProvider(DefaultProvider{Provider: "other"})
}

func TestRegisterDefaultProviderEmptyProviderPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("RegisterDefaultProvider with empty Provider should panic")
		}
	}()
	RegisterDefaultProvider(DefaultProvider{ID: "x"})
}
