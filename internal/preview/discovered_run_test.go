package preview

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

func TestDiscoveredGraphUsesDesiredChildrenAndRemovals(t *testing.T) {
	backend := graphBackend{render: func(_ context.Context, app domain.Application, rev string) ([]string, error) {
		if app.ID.Name == "root" {
			child := graphApp("child")
			child.Source()["helm"] = map[string]any{"releaseName": "desired"}
			out := []string{appManifest(child)}
			if rev == graphBase {
				out = append(out, appManifest(graphApp("removed")))
			}
			return out, nil
		}
		return []string{configMap(rev)}, nil
	}}
	runner := graphRunner(backend)
	runner.Config.Discovery = &profile.Discovery{Mode: "repository"}
	runner.Config.Instances = []profile.Instance{{ID: "one"}, {ID: "denied"}}
	inv := &fakeInventory{}
	for _, name := range []string{"child", "removed", "root"} {
		inv.apps = append(inv.apps, domain.OwnedApplication{Application: graphApp(name)})
	}
	report := runner.RunDiscovered(context.Background(), graphPair(), map[string]domain.ApplicationInventory{"one": inv})
	if report.Status != "partial_failure" || report.Coverage != "repository-discovered-target-graph" {
		t.Fatalf("unexpected status: %+v", report)
	}
	child := findApp(t, report, "child")
	if child.Execution != "complete" || len(child.HeadTarget.GraphPath) != 2 {
		t.Fatalf("stored child incorrectly became root: %+v", child)
	}
	if got := findApp(t, report, "removed"); got.State != "removed" {
		t.Fatalf("removed child retained as root: %+v", got)
	}
	if len(report.Discovery) != 2 || report.Discovery[0].Status != "failed" || report.Discovery[1].Status != "complete" {
		t.Fatal("instance coverage lost")
	}
}

func TestDiscoveredNoMatches(t *testing.T) {
	runner := graphRunner(graphBackend{})
	runner.Config.Discovery = &profile.Discovery{Mode: "repository"}
	runner.Config.Instances = []profile.Instance{{ID: "one"}}
	report := runner.RunDiscovered(context.Background(), graphPair(), map[string]domain.ApplicationInventory{"one": &fakeInventory{}})
	if len(report.Applications) != 0 || len(report.Discovery) != 1 || report.Discovery[0].Status != "no_matches" {
		t.Fatal("empty scope must be explicit")
	}
}

// Unaffected candidates are never rendered, are recorded with a reason, and do
// not make graph coverage incomplete.
func TestDiscoveredChangedPathSelectionSkipsOnlyProvenUnaffected(t *testing.T) {
	rendered := map[string]int{}
	var mu sync.Mutex
	backend := graphBackend{render: func(_ context.Context, app domain.Application, rev string) ([]string, error) {
		mu.Lock()
		rendered[app.ID.Name]++
		mu.Unlock()
		if app.ID.Name == "changed" {
			return []string{configMap(rev)}, nil
		}
		return []string{configMap("same")}, nil
	}}
	runner := graphRunner(backend)
	runner.Config.Discovery = &profile.Discovery{Mode: "repository", Selection: profile.SelectionChangedPaths}
	runner.Config.Instances = []profile.Instance{{ID: "one"}}
	chart := []byte("apiVersion: v2\nname: x\nversion: 0.1.0\n")
	runner.ChangedPaths = &ChangedPaths{Listed: true, Files: []string{"charts/changed/values.yaml"}, Read: func(_ context.Context, file string) ([]byte, bool, error) {
		return chart, file != "" && file[len(file)-len("Chart.yaml"):] == "Chart.yaml", nil
	}}
	no := false
	inv := &fakeInventory{}
	for _, name := range []string{"changed", "quiet", "unknown"} {
		owned := domain.OwnedApplication{Application: graphApp(name)}
		if name != "unknown" {
			owned.ManagesApplications = &no
		}
		inv.apps = append(inv.apps, owned)
	}
	report := runner.RunDiscovered(context.Background(), graphPair(), map[string]domain.ApplicationInventory{"one": inv})
	if rendered["quiet"] != 0 || rendered["changed"] == 0 || rendered["unknown"] == 0 {
		t.Fatalf("render calls %v", rendered)
	}
	if len(report.Applications) != 2 || report.Discovery[0].Unaffected != 1 || !report.Graph.BaseComplete || !report.Graph.HeadComplete || report.Status != "success" || !report.ChangesFound {
		t.Fatalf("unexpected report: status=%s graph=%+v discovery=%+v", report.Status, report.Graph, report.Discovery)
	}
	selections := map[string]string{}
	for _, decision := range report.Discovery[0].Decisions {
		if decision.SelectionReason == "" {
			t.Fatalf("selection without reason: %+v", decision)
		}
		selections[decision.ID.Name] = decision.Selection
	}
	if selections["changed"] != selectionRendered || selections["quiet"] != selectionUnaffected || selections["unknown"] != selectionIneligible {
		t.Fatalf("selections %v", selections)
	}
	found := false
	for _, limitation := range report.Limitations {
		found = found || strings.HasPrefix(limitation, "Changed-path selection did not render")
	}
	if !found {
		t.Fatal("changed-path selection boundary missing from limitations")
	}
}

// When selection skips nothing, the boundary says every candidate rendered
// instead of "did not render 0 of N".
func TestDiscoveredChangedPathSelectionNamesFullRender(t *testing.T) {
	backend := graphBackend{render: func(_ context.Context, _ domain.Application, rev string) ([]string, error) {
		return []string{configMap(rev)}, nil
	}}
	runner := graphRunner(backend)
	runner.Config.Discovery = &profile.Discovery{Mode: "repository", Selection: profile.SelectionChangedPaths}
	runner.Config.Instances = []profile.Instance{{ID: "one"}}
	runner.ChangedPaths = &ChangedPaths{Listed: true, Files: []string{"charts/changed/values.yaml"}, Read: func(context.Context, string) ([]byte, bool, error) {
		return []byte("apiVersion: v2\nname: x\nversion: 0.1.0\n"), true, nil
	}}
	no := false
	report := runner.RunDiscovered(context.Background(), graphPair(), map[string]domain.ApplicationInventory{"one": &fakeInventory{apps: []domain.OwnedApplication{{Application: graphApp("changed"), ManagesApplications: &no}}}})
	var notice string
	for _, limitation := range report.Limitations {
		if strings.HasPrefix(limitation, "Changed-path selection") {
			notice = limitation
		}
	}
	if !strings.HasPrefix(notice, "Changed-path selection rendered all 1 repository candidates") {
		t.Fatalf("selection notice %q", notice)
	}
}

func TestDiscoveredChangedPathSelectionAllUnaffectedIsNotNoMatch(t *testing.T) {
	runner := graphRunner(graphBackend{render: func(context.Context, domain.Application, string) ([]string, error) {
		t.Fatal("unaffected candidate rendered")
		return nil, nil
	}})
	runner.Config.Discovery = &profile.Discovery{Mode: "repository", Selection: profile.SelectionChangedPaths}
	runner.Config.Instances = []profile.Instance{{ID: "one"}}
	runner.ChangedPaths = &ChangedPaths{Listed: true, Files: []string{"docs/readme.md"}, Read: func(context.Context, string) ([]byte, bool, error) {
		return []byte("apiVersion: v2\nname: x\nversion: 0.1.0\n"), true, nil
	}}
	no := false
	inv := &fakeInventory{apps: []domain.OwnedApplication{{Application: graphApp("quiet"), ManagesApplications: &no}}}
	report := runner.RunDiscovered(context.Background(), graphPair(), map[string]domain.ApplicationInventory{"one": inv})
	if report.ExitCode() != 0 || report.Discovery[0].Status != "complete" || report.Discovery[0].Unaffected != 1 {
		t.Fatalf("unexpected report: exit=%d %+v", report.ExitCode(), report.Discovery)
	}
	for _, limitation := range report.Limitations {
		if len(limitation) > 30 && limitation[:30] == "No readable Application matche" {
			t.Fatal("unaffected candidates were reported as no matches")
		}
	}
}

// Repository candidates read their chart metadata concurrently within
// render.maxConcurrency; decisions keep the inventory order.
func TestChangedPathCandidatesAreDecidedConcurrently(t *testing.T) {
	var active, peak atomic.Int32
	runner := graphRunner(graphBackend{render: func(_ context.Context, _ domain.Application, rev string) ([]string, error) {
		return []string{configMap(rev)}, nil
	}})
	runner.Config.Render.MaxConcurrency = 3
	runner.Config.Discovery = &profile.Discovery{Mode: "repository", Selection: profile.SelectionChangedPaths}
	runner.Config.Instances = []profile.Instance{{ID: "one"}}
	runner.ChangedPaths = &ChangedPaths{Files: []string{"docs/readme.md"}, Listed: true, Read: func(context.Context, string) ([]byte, bool, error) {
		now := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); now > old && !peak.CompareAndSwap(old, now); old = peak.Load() {
		}
		time.Sleep(20 * time.Millisecond)
		return []byte("apiVersion: v2\nname: x\nversion: 0.1.0\n"), true, nil
	}}
	no := false
	inv := &fakeInventory{}
	for i := range 8 {
		inv.apps = append(inv.apps, domain.OwnedApplication{Application: graphApp(fmt.Sprintf("app-%d", i)), ManagesApplications: &no})
	}
	report := runner.RunDiscovered(context.Background(), graphPair(), map[string]domain.ApplicationInventory{"one": inv})
	if got := peak.Load(); got < 2 || got > 3 {
		t.Fatalf("peak concurrent chart reads %d, want 2..3", got)
	}
	decisions := report.Discovery[0].Decisions
	if len(decisions) != 8 || report.Discovery[0].Unaffected != 8 {
		t.Fatalf("decisions %+v", report.Discovery[0])
	}
	for i, decision := range decisions {
		if decision.ID.Name != fmt.Sprintf("app-%d", i) || decision.Selection != selectionUnaffected {
			t.Fatalf("decision %d: %+v", i, decision)
		}
	}
}

// Changed-path selection and inputChanges of one comparison read each head
// chart metadata file once, and only files the head tree holds.
func TestHeadChartMetadataIsReadOncePerComparison(t *testing.T) {
	charts := map[string]string{
		"charts/api/Chart.yaml":   "apiVersion: v2\nname: api\nversion: 0.1.0\ndependencies:\n  - name: lib\n    repository: file://../lib\n",
		"charts/lib/Chart.yaml":   "apiVersion: v2\nname: lib\nversion: 0.1.0\n",
		"charts/quiet/Chart.yaml": "apiVersion: v2\nname: quiet\nversion: 0.1.0\n",
	}
	changed := []string{"charts/lib/templates/_helpers.tpl"}
	reads := map[string]int{}
	var mu sync.Mutex
	read := func(revision, file string) ([]byte, bool, error) {
		mu.Lock()
		reads[revision+":"+file]++
		mu.Unlock()
		data, ok := charts[file]
		return []byte(data), ok, nil
	}
	runner := graphRunner(graphBackend{render: func(_ context.Context, _ domain.Application, rev string) ([]string, error) {
		return []string{configMap(rev)}, nil
	}})
	runner.Config.Discovery = &profile.Discovery{Mode: "repository", Selection: profile.SelectionChangedPaths}
	runner.Config.Instances = []profile.Instance{{ID: "one"}}
	present := map[string]bool{changed[0]: true}
	for file := range charts {
		present[file] = true
	}
	runner.ChangedPaths = &ChangedPaths{Files: changed, Listed: true, Present: present, Read: func(_ context.Context, file string) ([]byte, bool, error) {
		return read(graphHead, file)
	}}
	no := false
	inv := &fakeInventory{apps: []domain.OwnedApplication{{Application: graphApp("api"), ManagesApplications: &no}, {Application: graphApp("quiet"), ManagesApplications: &no}}}
	pair := graphPair()
	pair.Mode = domain.RevisionModePR
	report := runner.Compare(context.Background(), pair, false, map[string]domain.ApplicationInventory{"one": inv}, ComparisonEvidence{Files: &InputFiles{Files: changed, Read: func(_ context.Context, revision, file string) ([]byte, bool, error) {
		return read(revision, file)
	}}})
	api := findApp(t, report, "api")
	if len(report.Applications) != 1 || len(api.InputChanges) != 1 || api.InputChanges[0].Path != changed[0] || api.InputChanges[0].Kind != "chartFile" {
		t.Fatalf("dependency change not listed: %+v", report.Applications)
	}
	for _, file := range []string{"charts/api/Chart.yaml", "charts/lib/Chart.yaml", "charts/quiet/Chart.yaml"} {
		if reads[graphHead+":"+file] != 1 {
			t.Fatalf("head %s read %d times: %v", file, reads[graphHead+":"+file], reads)
		}
	}
	for key, n := range reads {
		if revision, file, _ := strings.Cut(key, ":"); revision == graphHead && !present[file] {
			t.Fatalf("head file absent from the tree was read %d times: %s", n, file)
		}
	}
	if reads[graphBase+":charts/api/Chart.yaml"] != 1 || reads[graphBase+":charts/lib/Chart.yaml"] != 1 {
		t.Fatalf("base dependencies not followed: %v", reads)
	}
	t.Logf("File calls: %d %v", len(reads), reads)
}

// An Application that reads the PR repository through an undeclared mirror
// host is not selected; the report names it first, with the setting that
// would select it. Declaring the mirror selects it and drops the hint.
func TestDiscoveredRunNamesUndeclaredMirrorSources(t *testing.T) {
	mirrored := graphApp("mirrored")
	mirrored.Source()["repoURL"] = "https://git-mirror.example.com/example/charts"
	unrelated := graphApp("unrelated")
	unrelated.Source()["repoURL"] = "https://github.com/example/other.git"
	backend := graphBackend{render: func(context.Context, domain.Application, string) ([]string, error) {
		return []string{configMap("stable")}, nil
	}}
	run := func(mirrorHosts ...string) domain.Report {
		runner := graphRunner(backend)
		runner.Config.Discovery = &profile.Discovery{Mode: "repository"}
		runner.Config.Instances = []profile.Instance{{ID: "one"}}
		runner.Config.Git.MirrorHosts = mirrorHosts
		inv := &fakeInventory{apps: []domain.OwnedApplication{{Application: mirrored}, {Application: unrelated}}}
		return runner.RunDiscovered(context.Background(), graphPair(), map[string]domain.ApplicationInventory{"one": inv})
	}
	report := run()
	if len(report.Applications) != 0 || len(report.Limitations) == 0 {
		t.Fatalf("apps=%d limitations=%v", len(report.Applications), report.Limitations)
	}
	hint := report.Limitations[0]
	if !strings.HasPrefix(hint, "1 Application(s) read the PR repository from https://git-mirror.example.com/example/charts and were not selected; add that host to git.mirrorHosts (init --mirror-host)") || !strings.Contains(hint, "https://git-mirror.example.com/example/charts") || !strings.Contains(hint, "--mirror-host") || strings.Contains(hint, "example/other") {
		t.Fatalf("first limitation does not name the undeclared mirror: %q", hint)
	}
	report = run("https://git-mirror.example.com")
	if len(report.Applications) != 1 || report.Applications[0].ID.Name != "mirrored" {
		t.Fatalf("declared mirror did not select the Application: %+v", report.Applications)
	}
	for _, limitation := range report.Limitations {
		if strings.Contains(limitation, "were not selected") {
			t.Fatalf("hint kept after the mirror was declared: %q", limitation)
		}
	}
}
