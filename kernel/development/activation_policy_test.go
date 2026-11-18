package development

import (
	"context"
	"strings"
	"testing"
	"time"

	"the8020/kernel/settings"
)

func TestActivationOverride(t *testing.T) {
	p := newTestPlatform(t)
	ctx := context.Background()
	for _, override := range []string{"false", "true", "auto"} {
		prepared, err := p.manager.Prepare(ctx, settings.Values{
			"development.idle_timeout":       settings.Duration(time.Hour),
			"development.activation_allowed": override,
		})
		if err != nil {
			t.Fatal(err)
		}
		prepared.Commit()
		// Discarded setting changes must not alter the active policy.
		discarded, err := p.manager.Prepare(ctx, settings.Values{
			"development.idle_timeout":       settings.Duration(time.Hour),
			"development.activation_allowed": "false",
		})
		if err != nil {
			t.Fatal(err)
		}
		discarded.Discard()
		if _, err := p.manager.EnsureSandbox(ctx, "alice"); err != nil {
			t.Fatalf("%s blocked sandbox access: %v", override, err)
		}
		for _, action := range []string{"", "continue", "status", "abort"} {
			_, err := p.manager.Activate(ctx, "alice", ActivationOptions{Action: action, Description: "Repair"})
			blocked := err != nil && strings.Contains(err.Error(), "development.activation_allowed=false")
			want := override == "false" && action != "status" && action != "abort"
			if blocked != want {
				t.Fatalf("%s action %q: blocked=%v, error=%v", override, action, blocked, err)
			}
		}
	}
	if _, err := p.manager.Prepare(ctx, settings.Values{
		"development.idle_timeout":       settings.Duration(time.Hour),
		"development.activation_allowed": "invalid",
	}); err == nil {
		t.Fatal("invalid override accepted")
	}
}
