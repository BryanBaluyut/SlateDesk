package settings_test

import (
	"context"
	"testing"

	"github.com/BryanBaluyut/slatedesk/internal/settings"
)

// TestSetupAccessors covers the M4 first-run wizard state accessors: absent
// keys read as the zero value (setup not done, empty name/URL), and writes
// round-trip. Kept as one sequential test because these are process-global
// singleton settings keys.
func TestSetupAccessors(t *testing.T) {
	ctx := context.Background()
	s := settings.New(testPool)

	// Defaults on an empty settings table.
	if done, err := s.SetupCompleted(ctx); err != nil || done {
		t.Fatalf("SetupCompleted default = (%v, %v), want (false, nil)", done, err)
	}
	if name, err := s.InstanceName(ctx); err != nil || name != "" {
		t.Fatalf("InstanceName default = (%q, %v), want (\"\", nil)", name, err)
	}
	if url, err := s.ExternalURL(ctx); err != nil || url != "" {
		t.Fatalf("ExternalURL default = (%q, %v), want (\"\", nil)", url, err)
	}

	// Write, then read back.
	if err := s.SetInstanceName(ctx, "Acme Support"); err != nil {
		t.Fatalf("SetInstanceName: %v", err)
	}
	if err := s.SetExternalURL(ctx, "https://desk.acme.test"); err != nil {
		t.Fatalf("SetExternalURL: %v", err)
	}
	if err := s.SetSetupCompleted(ctx, true); err != nil {
		t.Fatalf("SetSetupCompleted: %v", err)
	}

	if done, err := s.SetupCompleted(ctx); err != nil || !done {
		t.Errorf("SetupCompleted after set = (%v, %v), want (true, nil)", done, err)
	}
	if name, err := s.InstanceName(ctx); err != nil || name != "Acme Support" {
		t.Errorf("InstanceName after set = (%q, %v)", name, err)
	}
	if url, err := s.ExternalURL(ctx); err != nil || url != "https://desk.acme.test" {
		t.Errorf("ExternalURL after set = (%q, %v)", url, err)
	}

	// Idempotent completion (the wizard may finish more than once).
	if err := s.SetSetupCompleted(ctx, true); err != nil {
		t.Fatalf("SetSetupCompleted (repeat): %v", err)
	}
	if done, err := s.SetupCompleted(ctx); err != nil || !done {
		t.Errorf("SetupCompleted after repeat = (%v, %v), want (true, nil)", done, err)
	}
}
