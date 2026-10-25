package preview

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

// instanceApp is graphApp on another Argo CD instance.
func instanceApp(instance, name string) domain.OwnedApplication {
	app := graphApp(name)
	app.ID.Instance = instance
	return domain.OwnedApplication{Application: app}
}

// A failed discovery instance renders none of its candidates, so none of its
// decisions says selected, and its decisions encode as a list, never null.
// Each failure kind keeps whatever decisions it could make.
func TestFailedDiscoveryInstanceSelectsNothing(t *testing.T) {
	large := instanceApp("two", "large")
	large.Application.Spec["padding"] = strings.Repeat("x", domain.RetainedByteLimit)
	for _, tc := range []struct {
		name      string
		inventory domain.ApplicationInventory
		// decisions is how many decisions the failed instance keeps.
		decisions int
	}{
		{"combined candidate limit", &fakeInventory{apps: []domain.OwnedApplication{instanceApp("two", "api")}}, 1},
		{"invalid inventory", &fakeInventory{apps: []domain.OwnedApplication{instanceApp("one", "api")}}, 0},
		{"inventory count limit", &fakeInventory{apps: []domain.OwnedApplication{instanceApp("two", "api"), instanceApp("two", "worker")}}, 0},
		{"inventory byte limit", &fakeInventory{apps: []domain.OwnedApplication{large}}, 1},
		{"inventory read denied", &fakeInventory{err: domain.Fail(domain.CodeAccessDenied, "permission", "denied", "grant read", nil)}, 0},
		{"inventory unavailable", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := graphRunner(graphBackend{render: func(_ context.Context, _ domain.Application, rev string) ([]string, error) {
				return []string{configMap(rev)}, nil
			}})
			runner.Config.Discovery = &profile.Discovery{Mode: "repository"}
			runner.Config.Instances = []profile.Instance{{ID: "one"}, {ID: "two"}}
			runner.Config.Render.MaxApplications = 1
			inventories := map[string]domain.ApplicationInventory{"one": &fakeInventory{apps: []domain.OwnedApplication{instanceApp("one", "root")}}}
			if tc.inventory != nil {
				inventories["two"] = tc.inventory
			}
			report := runner.RunDiscovered(context.Background(), graphPair(), inventories)
			if len(report.Discovery) != 2 || report.Discovery[0].Status != "complete" || !report.Discovery[0].Decisions[0].Selected {
				t.Fatalf("the independent instance must stay selected: %+v", report.Discovery)
			}
			failed := report.Discovery[1]
			if failed.Status != "failed" || len(failed.Decisions) != tc.decisions {
				t.Fatalf("status %s with %d decisions, want failed with %d", failed.Status, len(failed.Decisions), tc.decisions)
			}
			for _, decision := range failed.Decisions {
				if decision.Selected || decision.Reason != discoveryFailedReason {
					t.Fatalf("unrendered candidate claimed or lost its reason: %+v", decision)
				}
			}
			for _, app := range report.Applications {
				if app.ID.Instance == "two" {
					t.Fatalf("failed instance rendered %s", app.ID)
				}
			}
			encoded, err := json.Marshal(failed)
			if err != nil || !strings.Contains(string(encoded), `"decisions":[`) {
				t.Fatalf("decisions must encode as a list: %s", encoded)
			}
		})
	}
}
