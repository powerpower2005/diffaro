package preview

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"diffaro/internal/domain"
)

func discoveryApp(name, repo, project string) domain.OwnedApplication {
	return domain.OwnedApplication{Application: domain.Application{ID: domain.ApplicationID{Instance: "one", Namespace: "argocd", Name: name}, Spec: map[string]any{"project": project, "source": map[string]any{"repoURL": repo, "path": "charts/" + name, "helm": map[string]any{}}}}}
}

func TestRepositoryDiscoverySelectionAndProvenance(t *testing.T) {
	const repo = "https://github.com/example/charts.git"
	a := discoveryApp("api", repo, "team")
	b := discoveryApp("worker", "https://github.com/example/charts/", "team")
	c := discoveryApp("other", "https://github.com/other/charts.git", "team")
	d := discoveryApp("outside", repo, "other")
	e := discoveryApp("multi", "https://github.com/example/else.git", "team")
	e.Application.Spec["sources"] = []any{map[string]any{"repoURL": repo, "ref": "values"}}
	inv := &fakeInventory{apps: []domain.OwnedApplication{e, d, c, b, a}}
	scope := discoveryScope{Instance: "one", Projects: []string{"team"}}
	got, err := discoverRepository(context.Background(), scope, repo, inv, 10)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, app := range got.Applications {
		names = append(names, app.ID.Name)
	}
	if !reflect.DeepEqual(names, []string{"api", "multi", "worker"}) {
		t.Fatalf("shared inputs or multi-source candidate lost: %v", names)
	}
	if len(got.Decisions) != 5 || got.Decisions[2].Reason != "different-source-repository" || got.Decisions[3].Reason != "outside-project-filter" {
		t.Fatal("selection reasons missing")
	}
	if err := verifyDiscovery(context.Background(), got, repo, inv, 10); err != nil {
		t.Fatal(err)
	}
	// A previously unrelated app joining the repository invalidates the result.
	c.Application.Source()["repoURL"] = repo
	if err := verifyDiscovery(context.Background(), got, repo, inv, 10); err == nil {
		t.Fatal("newly relevant inventory drift hidden")
	}
	if domain.String(got.Applications[0].Source(), "repoURL") != repo {
		t.Fatal("frozen snapshot changed")
	}
}

func TestRepositoryDiscoveryBoundaries(t *testing.T) {
	const repo = "https://github.com/example/charts.git"
	a := discoveryApp("api", repo, "team")
	for _, tc := range []struct {
		name      string
		inventory domain.ApplicationInventory
		limit     int
		wantError bool
	}{
		{"empty", &fakeInventory{}, 10, false},
		{"missing", nil, 10, true},
		{"denied", &fakeInventory{err: domain.Fail("access_denied", "permission", "denied", "grant read", nil)}, 10, true},
		{"limit", &fakeInventory{apps: []domain.OwnedApplication{a}}, 0, true},
		{"overflow", &fakeInventory{apps: []domain.OwnedApplication{a, a}}, 1, true},
		{"duplicate", &fakeInventory{apps: []domain.OwnedApplication{a, a}}, 10, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := discoverRepository(context.Background(), discoveryScope{Instance: "one"}, repo, tc.inventory, tc.limit)
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected boundary result: %v", err)
			}
			if tc.name == "empty" && (len(got.Applications) != 0 || got.Decisions == nil) {
				t.Fatal("empty inventory result invalid")
			}
		})
	}
	wrong := discoveryApp("api", repo, "team")
	wrong.Application.ID.Instance = "other"
	if _, err := discoverRepository(context.Background(), discoveryScope{Instance: "one"}, repo, &fakeInventory{apps: []domain.OwnedApplication{wrong}}, 10); err == nil {
		t.Fatal("cross-instance inventory accepted")
	}
}

// Verification fails only on changes that can alter selection or the frozen
// root specs; unrelated spec churn such as image updates is tolerated.
func TestDiscoveryVerificationIgnoresUnrelatedSpecChanges(t *testing.T) {
	const repo = "https://github.com/example/charts.git"
	inventory := func(mutate func(map[string]domain.OwnedApplication)) *fakeInventory {
		apps := map[string]domain.OwnedApplication{"api": discoveryApp("api", repo, "team"), "other": discoveryApp("other", "https://github.com/example/other.git", "team")}
		mutate(apps)
		out := &fakeInventory{}
		for _, name := range []string{"api", "added", "other"} {
			if app, ok := apps[name]; ok {
				out.apps = append(out.apps, app)
			}
		}
		return out
	}
	before, err := discoverRepository(context.Background(), discoveryScope{Instance: "one"}, repo, inventory(func(map[string]domain.OwnedApplication) {}), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		mutate  func(map[string]domain.OwnedApplication)
		changed bool
	}{
		{"unrelated parameter", func(a map[string]domain.OwnedApplication) {
			a["other"].Application.Source()["helm"] = map[string]any{"parameters": []any{map[string]any{"name": "image.tag", "value": "v2"}}}
		}, false},
		{"unrelated app switches to the PR repository", func(a map[string]domain.OwnedApplication) {
			a["other"].Application.Source()["repoURL"] = repo
		}, true},
		{"unrelated project", func(a map[string]domain.OwnedApplication) { a["other"].Application.Spec["project"] = "moved" }, true},
		{"candidate spec", func(a map[string]domain.OwnedApplication) { a["api"].Application.Source()["path"] = "charts/new" }, true},
		{"membership", func(a map[string]domain.OwnedApplication) {
			a["added"] = discoveryApp("added", "https://github.com/example/other.git", "team")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyDiscovery(context.Background(), before, repo, inventory(tc.mutate), 10)
			var failure *domain.Failure
			if tc.changed != (errors.As(err, &failure) && failure.Issue.Code == "selection_changed") {
				t.Fatalf("changed=%v err=%v", tc.changed, err)
			}
		})
	}
}
