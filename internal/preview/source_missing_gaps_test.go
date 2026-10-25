package preview

import (
	"context"
	"strings"
	"sync"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

// Source absence requires a single-source candidate. Argo CD uses `sources`
// when it is non-empty, so a leftover `source` whose path is missing proves
// nothing about what renders.
func TestG68SourcesListBlocksSourceMissing(t *testing.T) {
	read := func(context.Context, string) ([]byte, bool, error) { return nil, false, nil }
	present := map[string]bool{"charts/api/Chart.yaml": true}
	paths := ChangedPaths{Files: []string{"charts/api/values.yaml"}, Read: read, Listed: true, Present: present}

	single := pathApp(helmSource("charts/removed", nil))
	selector := newChangedPathSelector(paths, profile.Git{RepositoryURL: pathRepo}.Matches)
	if got, _ := selector.decide(context.Background(), single, nil); got != selectionSourceMissing {
		t.Fatalf("fixture precondition: single-source candidate selection=%s, want %s", got, selectionSourceMissing)
	}

	both := pathApp(helmSource("charts/removed", nil))
	both.Spec["sources"] = []any{helmSource("charts/api", map[string]any{})}
	selector = newChangedPathSelector(paths, profile.Git{RepositoryURL: pathRepo}.Matches)
	got, reason := selector.decide(context.Background(), both, nil)
	if got == selectionSourceMissing {
		t.Fatalf("an Application with a non-empty sources list was called source-missing: %s", reason)
	}
}

// A source path is missing only when no head file is the path or lies under
// it; a file in a sibling directory sharing its name prefix does not count.
func TestSourceMissingNeedsNoHeadFileUnderThePath(t *testing.T) {
	read := func(context.Context, string) ([]byte, bool, error) { return nil, false, nil }
	present := map[string]bool{"charts/api/Chart.yaml": true, "charts/deep/a/b/values.yaml": true, "charts/file": true, "charts/app-v2/Chart.yaml": true, "top.yaml": true}
	paths := ChangedPaths{Files: []string{"charts/api/values.yaml"}, Read: read, Listed: true, Present: present}
	selector := newChangedPathSelector(paths, profile.Git{RepositoryURL: pathRepo}.Matches)
	for path, missing := range map[string]bool{"charts/deep": false, "charts/deep/a": false, "charts/deep/a/b": false, "charts/file": false, "charts/app": true, "charts/deep/a/b/c": true, "other": true} {
		got, reason := selector.decide(context.Background(), pathApp(helmSource(path, nil)), nil)
		if (got == selectionSourceMissing) != missing {
			t.Fatalf("source path %s: selection=%s (%s), missing %v", path, got, reason, missing)
		}
	}
}

// Source absence requires a relative path inside the repository.
// A path that is absolute or leaves the repository is invalid input, not a
// missing chart: the candidate stays rendered so the backend reports the
// failure on both revisions instead of the run hiding it as source-missing.
func TestInvalidSourcePathIsNotSourceMissing(t *testing.T) {
	read := func(context.Context, string) ([]byte, bool, error) { return nil, false, nil }
	paths := ChangedPaths{Files: []string{"charts/api/values.yaml"}, Read: read, Listed: true, Present: map[string]bool{"charts/api/Chart.yaml": true}}
	for _, tc := range []struct {
		path string
		want string
	}{
		{"..", selectionIneligible},
		{"a/../..", selectionIneligible},
		{"/x", selectionIneligible},
		{"charts/removed", selectionSourceMissing},
	} {
		t.Run(tc.path, func(t *testing.T) {
			selector := newChangedPathSelector(paths, profile.Git{RepositoryURL: pathRepo}.Matches)
			got, reason := selector.decide(context.Background(), pathApp(helmSource(tc.path, nil)), nil)
			if got != tc.want {
				t.Fatalf("source path %q: selection=%s (%s), want %s", tc.path, got, reason, tc.want)
			}
		})
	}
}

// relativeSourceDir is the one source-path check shared by selection,
// source-missing proof and input collection; every site must reject the
// same escaping paths.
func TestRelativeSourceDir(t *testing.T) {
	for _, tc := range []struct {
		path string
		dir  string
		ok   bool
	}{
		{"charts/api", "charts/api", true},
		{".", "", true},
		{"", "", false},
		{"..", "", false},
		{"../x", "", false},
		{"a/../..", "", false},
		{"/x", "", false},
		{"charts/api/", "", false},
		{"charts\\api", "", false},
	} {
		dir, ok := relativeSourceDir(tc.path)
		if dir != tc.dir || ok != tc.ok {
			t.Errorf("relativeSourceDir(%q) = (%q, %t), want (%q, %t)", tc.path, dir, ok, tc.dir, tc.ok)
		}
	}
}

// A source-missing candidate in a discovered run is
// never rendered, counts as unaffected, records that its inputs are
// unchanged and does not turn the run into a partial failure.
func TestG68DiscoveredRunSkipsSourceMissingCandidate(t *testing.T) {
	rendered := map[string]int{}
	var mu sync.Mutex
	backend := graphBackend{render: func(_ context.Context, app domain.Application, rev string) ([]string, error) {
		mu.Lock()
		rendered[app.ID.Name]++
		mu.Unlock()
		if app.ID.Name == "orphan" {
			// What Argo CD returns for a path that exists at neither revision.
			return nil, domain.Fail("backend_error", "render", "app path does not exist", "check the source path", nil)
		}
		return []string{configMap(rev)}, nil
	}}
	runner := graphRunner(backend)
	runner.Config.Discovery = &profile.Discovery{Mode: "repository", Selection: profile.SelectionChangedPaths}
	runner.Config.Instances = []profile.Instance{{ID: "one"}}
	chart := []byte("apiVersion: v2\nname: changed\nversion: 0.1.0\n")
	runner.ChangedPaths = &ChangedPaths{
		Listed:  true,
		Present: map[string]bool{"charts/changed/Chart.yaml": true, "charts/changed/values.yaml": true},
		Files:   []string{"charts/changed/values.yaml"},
		Read: func(_ context.Context, file string) ([]byte, bool, error) {
			if strings.HasSuffix(file, "Chart.yaml") {
				return chart, true, nil
			}
			return []byte("{}\n"), true, nil
		},
	}
	no := false
	// The orphan's child-Application status is unknown, so without the
	// absence proof it would be ineligible and rendered.
	inv := &fakeInventory{apps: []domain.OwnedApplication{
		{Application: graphApp("changed"), ManagesApplications: &no},
		{Application: graphApp("orphan")},
	}}
	report := runner.RunDiscovered(context.Background(), graphPair(), map[string]domain.ApplicationInventory{"one": inv})

	if rendered["orphan"] != 0 {
		t.Fatalf("source-missing candidate was rendered %d times", rendered["orphan"])
	}
	if rendered["changed"] == 0 {
		t.Fatal("fixture precondition: the changed candidate was not rendered")
	}
	if len(report.Discovery) != 1 || report.Discovery[0].Unaffected != 1 {
		t.Fatalf("source-missing candidate not counted as unaffected: %+v", report.Discovery)
	}
	var decision *domain.DiscoveryDecision
	for i := range report.Discovery[0].Decisions {
		if report.Discovery[0].Decisions[i].ID.Name == "orphan" {
			decision = &report.Discovery[0].Decisions[i]
		}
	}
	if decision == nil || decision.Selection != selectionSourceMissing || !strings.Contains(decision.SelectionReason, "charts/orphan/") {
		t.Fatalf("orphan decision: %+v", decision)
	}
	if !decision.InputsUnchanged {
		t.Fatalf("source-missing decision must record inputsUnchanged: %+v", decision)
	}
	for _, app := range report.Applications {
		if app.ID.Name == "orphan" {
			t.Fatalf("source-missing candidate appears as a compared Application: %+v", app)
		}
	}
	if report.ExitCode() == 3 || report.Status != "success" || len(report.Issues) != 0 {
		t.Fatalf("source-missing candidate caused a failure: status=%s exit=%d issues=%+v", report.Status, report.ExitCode(), report.Issues)
	}
}
