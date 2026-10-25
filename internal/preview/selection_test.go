package preview

import (
	"context"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

type fakeInventory struct {
	apps []domain.OwnedApplication
	err  error
}

func (f *fakeInventory) ListApplications(context.Context, string, int) ([]domain.OwnedApplication, error) {
	return f.apps, f.err
}

func TestGeneratedSelectionPreservesIndependentResultsAndDetectsDrift(t *testing.T) {
	id := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api"}
	source := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "fleet"}
	f := &fakeInventory{apps: []domain.OwnedApplication{{Application: domain.Application{ID: id, Spec: map[string]any{"project": "demo"}}, OwnerName: "fleet", OwnerUID: "123"}}}
	c := profile.Config{Applications: []domain.ApplicationID{id}, ApplicationSets: []domain.ApplicationID{source}, Render: profile.Render{MaxApplications: 10}}
	inventories := map[string]domain.ApplicationInventory{"test": f}
	resolved, selections, snapshots, issues := SelectGenerated(context.Background(), c, inventories)
	if len(resolved.Applications) != 1 || len(selections) != 1 || len(selections[0].Applications) != 1 || len(issues) != 0 {
		t.Fatal("selection duplicated an explicit root or lost provenance")
	}
	if len(VerifySelections(context.Background(), c, inventories, snapshots)) != 0 {
		t.Fatal("stable selection reported drift")
	}
	f.apps[0].Application.Spec["project"] = "changed"
	issues = VerifySelections(context.Background(), c, inventories, snapshots)
	if len(issues) != 1 || issues[0].Code != "selection_changed" {
		t.Fatal("spec drift was ignored")
	}
	r := domain.NewReport()
	r.Applications = []domain.ApplicationResult{{ID: id, Execution: "complete"}}
	ApplySelectionReport(&r, selections, issues)
	if r.Status != "partial_failure" || len(r.Applications) != 1 {
		t.Fatal("partial boundary lost independent results")
	}
	f.apps = nil
	_, _, _, issues = SelectGenerated(context.Background(), c, inventories)
	if len(issues) != 1 || issues[0].Code != "selection_empty" {
		t.Fatal("empty inventory was treated as no changes")
	}
}

func TestGeneratedSelectionRejectsAmbiguityDenialAndCombinedLimit(t *testing.T) {
	source := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "fleet"}
	app := domain.OwnedApplication{Application: domain.Application{ID: domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "a"}, Spec: map[string]any{}}, OwnerName: "fleet", OwnerUID: "1"}
	other := app
	other.Application.ID.Name = "b"
	other.OwnerUID = "2"
	for _, tc := range []struct {
		name      string
		inventory *fakeInventory
		max       int
		code      string
	}{
		{"different-controllers", &fakeInventory{apps: []domain.OwnedApplication{app, other}}, 10, "selection_ambiguous"},
		{"denied", &fakeInventory{err: domain.Fail("access_denied", "permission", "denied", "check token", nil)}, 10, "access_denied"},
		{"combined-limit", &fakeInventory{apps: []domain.OwnedApplication{app}}, 1, "selection_limit_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := profile.Config{Applications: []domain.ApplicationID{{Instance: "test", Namespace: "argocd", Name: "explicit"}}, ApplicationSets: []domain.ApplicationID{source}, Render: profile.Render{MaxApplications: tc.max}}
			resolved, _, _, issues := SelectGenerated(context.Background(), c, map[string]domain.ApplicationInventory{"test": tc.inventory})
			if len(issues) != 1 || issues[0].Code != tc.code || len(resolved.Applications) != 1 {
				t.Fatal("selection failed open or removed independent root")
			}
		})
	}
}

func TestSelectionDenialDoesNotInventPartialSuccess(t *testing.T) {
	r := domain.NewReport()
	r.Status = "denied"
	r.Applications = []domain.ApplicationResult{{State: "denied", Execution: "denied"}}
	ApplySelectionReport(&r, nil, []domain.Issue{{Code: "access_denied"}})
	if r.Status != "denied" || r.ExitCode() != 5 {
		t.Fatal("fully denied scope advertised partial success")
	}
	r.Applications[0].Execution = "failed"
	ApplySelectionReport(&r, nil, []domain.Issue{{Code: "selection_empty"}})
	if r.Status != "failed" || r.ExitCode() != 6 {
		t.Fatal("fully failed scope advertised partial success")
	}
}
