package preview

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/graph"
	"diffaro/internal/profile"
	"diffaro/internal/reporter"
	"diffaro/internal/security"
)

const graphRepo = "https://github.com/example/charts.git"
const graphBase = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const graphHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func graphApp(name string) domain.Application {
	return domain.Application{ID: domain.ApplicationID{Instance: "one", Namespace: "argocd", Name: name}, Spec: map[string]any{"project": "demo", "source": map[string]any{"repoURL": graphRepo, "path": "charts/" + name, "targetRevision": "main", "helm": map[string]any{}}, "destination": map[string]any{"name": "management", "namespace": "argocd"}}}
}
func appManifest(a domain.Application) string {
	b, _ := json.Marshal(map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", "metadata": map[string]any{"name": a.ID.Name, "namespace": a.ID.Namespace}, "spec": a.Spec})
	return string(b)
}
func configMap(value string) string {
	return `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings"},"data":{"setting":"` + value + `"}}`
}

type graphBackend struct {
	apps   map[string]domain.Application
	render func(context.Context, domain.Application, string) ([]string, error)
}

func (b graphBackend) Application(_ context.Context, id domain.ApplicationID) (domain.Application, error) {
	a, ok := b.apps[id.Name]
	if !ok {
		return a, domain.Fail("not_found", "lookup", "missing Application", "configure Application", nil)
	}
	return a, nil
}
func (b graphBackend) Render(ctx context.Context, a domain.Application, rev string) ([]string, error) {
	return b.render(ctx, a, rev)
}
func (b graphBackend) RenderDesired(ctx context.Context, a domain.Application, rev string) ([]string, error) {
	return b.render(ctx, a, rev)
}
func graphRunner(backend graphBackend, roots ...string) Runner {
	c := profile.Config{Git: profile.Git{RepositoryURL: graphRepo}, Routing: profile.Routing{DefaultInstance: "one"}, Render: profile.Render{Timeout: "1s", MaxConcurrency: 4, MaxApplications: 100, MaxDepth: 16, MaxResponseBytes: 1 << 20, MaxDocuments: 100}}
	for _, name := range roots {
		c.Applications = append(c.Applications, graphApp(name).ID)
	}
	return Runner{Config: c, Renderers: map[string]domain.Renderer{"one": backend}, Redactor: security.Redactor{}}
}
func graphPair() domain.RevisionPair {
	return domain.RevisionPair{Repository: "example/charts", Base: graphBase, Head: graphHead, Mode: "explicit-base-head"}
}
func findApp(t *testing.T, report domain.Report, name string) domain.ApplicationResult {
	t.Helper()
	for _, app := range report.Applications {
		if app.ID.Name == name {
			return app
		}
	}
	t.Fatalf("missing Application %s", name)
	return domain.ApplicationResult{}
}

func TestRecursiveAddedRemovedChangedAndPruning(t *testing.T) {
	b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, a domain.Application, revision string) ([]string, error) {
		switch a.ID.Name {
		case "root":
			child := "old"
			if revision == graphHead {
				child = "new"
			}
			return []string{appManifest(graphApp("shared")), appManifest(graphApp(child))}, nil
		case "shared":
			return []string{appManifest(graphApp("deep"))}, nil
		case "deep":
			return []string{configMap(revision)}, nil
		default:
			return []string{configMap("stable")}, nil
		}
	}}
	report := graphRunner(b, "root").RunRecursive(context.Background(), graphPair())
	if report.ExitCode() != 1 || !report.Graph.BaseComplete || !report.Graph.HeadComplete || len(report.Applications) != 5 {
		t.Fatalf("unexpected graph report: %+v", report)
	}
	for name, state := range map[string]string{"root": "changed", "shared": "unchanged", "deep": "changed", "old": "removed", "new": "added"} {
		if got := findApp(t, report, name); string(got.State) != state || got.Execution != "complete" {
			t.Fatalf("%s: %s/%s", name, got.State, got.Execution)
		}
	}
	old := findApp(t, report, "old")
	if old.Pruning == nil || old.Pruning.Certainty != "conditional" || len(old.Pruning.Resources) != 1 || old.Resources[0].State != "removed" {
		t.Fatal("removed target pruning candidates missing")
	}
	deep := findApp(t, report, "deep")
	if len(deep.BaseTarget.GraphPath) != 3 || deep.BaseTarget.Inputs.Origin != "parent-target-manifest" || deep.BaseTarget.Revision != graphBase || deep.HeadTarget.Revision != graphHead {
		t.Fatal("source or discovery provenance missing")
	}
}

func TestFailedDiscoveryDoesNotInventRemoval(t *testing.T) {
	b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root"), "independent": graphApp("independent")}, render: func(_ context.Context, a domain.Application, revision string) ([]string, error) {
		if a.ID.Name == "root" {
			if revision == graphHead {
				return nil, domain.Fail("backend_error", "render", "head render failed", "retry", nil)
			}
			return []string{appManifest(graphApp("child"))}, nil
		}
		return []string{configMap(revision)}, nil
	}}
	report := graphRunner(b, "root", "independent").RunRecursive(context.Background(), graphPair())
	child := findApp(t, report, "child")
	if report.ExitCode() != 3 || report.Graph.HeadComplete || child.State != "unknown" || child.Pruning != nil || len(child.Resources) != 0 {
		t.Fatal("missing failed branch was called removed")
	}
	if findApp(t, report, "independent").State != "changed" || !report.ChangesFound {
		t.Fatal("independent successful comparison lost")
	}
}

func TestRecursiveCrossRepositoryRevisionRules(t *testing.T) {
	for _, ref := range []string{graphBase, "main"} {
		t.Run(ref, func(t *testing.T) {
			child := graphApp("child")
			child.Source()["repoURL"], child.Source()["targetRevision"] = "https://github.com/example/other.git", ref
			var childRenders atomic.Int32
			b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, a domain.Application, revision string) ([]string, error) {
				if a.ID.Name == "root" {
					return []string{appManifest(child)}, nil
				}
				childRenders.Add(1)
				if revision != ref {
					t.Fatal("selected repository SHA was applied to another repository")
				}
				return []string{configMap("stable")}, nil
			}}
			report := graphRunner(b, "root").RunRecursive(context.Background(), graphPair())
			result := findApp(t, report, "child")
			if ref == "main" {
				if childRenders.Load() != 0 || report.Status != "partial_failure" || result.Issues[0].Code != "revision_mapping_required" {
					t.Fatal("mutable cross-repository revision silently accepted")
				}
				// The basic backend cannot resolve the branch; the action names
				// the backends that can, not an internal resolver.
				if action := result.Issues[0].Action; !strings.Contains(action, "--local-helm") || !strings.Contains(action, "repo-server") || strings.Contains(action, "resolver") {
					t.Fatalf("action does not say how to compare this source: %q", action)
				}
			} else if childRenders.Load() != 1 || report.Status != "success" || result.BaseTarget.RevisionRule != "source-commit" || result.HeadTarget.Revision != graphBase {
				// The pinned source renders once and serves both revisions.
				t.Fatal("pinned source mapping or report lost")
			}
		})
	}
}

// A child shares one render between base and head exactly when it renders
// at its own source commit; a repository alias of the PR repository renders
// per revision like the PR repository itself.
func TestRecursiveChildSharesRenderOnlyAtItsOwnCommit(t *testing.T) {
	for _, tc := range []struct {
		name, repoURL string
		shared        bool
	}{
		{"PR repository", graphRepo, false},
		{"PR repository alias", "https://mirror.example.test/example/charts.git", false},
		{"other repository", "https://github.com/example/other.git", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := graphApp("child")
			child.Source()["repoURL"], child.Source()["targetRevision"] = tc.repoURL, graphBase
			var childRenders atomic.Int32
			b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, a domain.Application, revision string) ([]string, error) {
				if a.ID.Name == "root" {
					return []string{appManifest(child)}, nil
				}
				childRenders.Add(1)
				return []string{configMap(revision)}, nil
			}}
			runner := graphRunner(b, "root")
			runner.Config.Git.RepositoryAliases = []string{"https://mirror.example.test/example/charts.git"}
			result := findApp(t, runner.RunRecursive(context.Background(), graphPair()), "child")
			ownCommit := result.BaseTarget.RevisionRule == "source-commit" && result.HeadTarget.RevisionRule == "source-commit"
			if ownCommit != tc.shared || (childRenders.Load() == 1) != tc.shared {
				t.Fatalf("revision rule %s, %d child renders; want shared=%v", result.BaseTarget.RevisionRule, childRenders.Load(), tc.shared)
			}
		})
	}
}

type resolvingGraphBackend struct {
	graphBackend
	resolutions *atomic.Int32
}

func (b resolvingGraphBackend) ResolveSourceRevision(context.Context, domain.Application, string, string) (string, error) {
	b.resolutions.Add(1)
	return strings.Repeat("c", 40), nil
}

// A single-source child from another repository follows the same non-PR
// revision rules as multi-source plans: tagPrefix is never resolved
// automatically, even when a resolver is configured.
func TestRecursiveCrossRepositoryTagPrefixRequiresPinnedCommit(t *testing.T) {
	for _, tagPrefix := range []string{"", "v"} {
		t.Run("tagPrefix="+tagPrefix, func(t *testing.T) {
			child := graphApp("child")
			child.Source()["repoURL"] = "https://github.com/example/other.git"
			if tagPrefix != "" {
				child.Source()["tagPrefix"] = tagPrefix
			}
			var childRenders, resolutions atomic.Int32
			b := resolvingGraphBackend{graphBackend: graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, a domain.Application, revision string) ([]string, error) {
				if a.ID.Name == "root" {
					return []string{appManifest(child)}, nil
				}
				childRenders.Add(1)
				return []string{configMap(revision)}, nil
			}}, resolutions: &resolutions}
			r := graphRunner(b.graphBackend, "root")
			r.Renderers["one"] = b
			report := r.RunRecursive(context.Background(), graphPair())
			result := findApp(t, report, "child")
			if tagPrefix == "" {
				// Control: without tagPrefix the resolver freezes the reference.
				if resolutions.Load() == 0 || result.Execution != "complete" || result.BaseTarget.RevisionRule != "resolved-source-reference" || result.HeadTarget.Revision != strings.Repeat("c", 40) || result.BaseTarget.RequestedRevision != "main" || result.HeadTarget.RequestedRevision != "main" {
					t.Fatalf("resolver path lost: %+v", result)
				}
				return
			}
			if resolutions.Load() != 0 || childRenders.Load() != 0 || result.Execution == "complete" || report.Status != "partial_failure" || len(result.Issues) == 0 {
				t.Fatalf("tagPrefix source was resolved automatically: %+v", result)
			}
			issue := result.Issues[0]
			if issue.Code != "revision_mapping_required" || issue.Stage != "revision" || issue.SourcePosition != 0 || issue.Repository != "https://github.com/example/other.git" || issue.Revision != "main" {
				t.Fatalf("unexpected issue: %+v", issue)
			}
		})
	}
}

func TestRecursiveSecretsAndResultBudget(t *testing.T) {
	b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, a domain.Application, revision string) ([]string, error) {
		if a.ID.Name == "root" {
			child := graphApp("child")
			child.Source()["helm"] = map[string]any{"values": "fictional-sensitive-" + revision}
			return []string{appManifest(child)}, nil
		}
		return []string{configMap("safe")}, nil
	}}
	runner := graphRunner(b, "root")
	report := runner.RunRecursive(context.Background(), graphPair())
	if findApp(t, report, "child").State != "changed" {
		t.Fatal("masked spec change was lost")
	}
	for _, format := range []string{"json", "markdown"} {
		output, err := reporter.Encode(report, format, security.Redactor{})
		if err != nil || strings.Contains(string(output), "fictional-sensitive-") {
			t.Fatal("child inline values leaked in report")
		}
	}
	runner.maxResultBytes = 1
	report = runner.RunRecursive(context.Background(), graphPair())
	if report.ExitCode() != 6 {
		t.Fatal("report exhaustion reported as success")
	}
	for _, app := range report.Applications {
		if len(app.Resources) != 0 || len(app.SpecChanges) != 0 || app.BaseTarget != nil || app.HeadTarget != nil {
			t.Fatal("excess results retained")
		}
	}
}

func TestRecursiveTimeoutAndCancellation(t *testing.T) {
	b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(ctx context.Context, _ domain.Application, _ string) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	runner := graphRunner(b, "root")
	runner.Config.Render.Timeout = "1ms"
	report := runner.RunRecursive(context.Background(), graphPair())
	if report.ExitCode() != 6 || report.Issues[0].Code != "timeout" {
		t.Fatal("timeout lost safe graph context")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if report = runner.RunRecursive(ctx, graphPair()); report.ExitCode() != 7 {
		t.Fatal("cancelled graph status lost")
	}
}

func TestPartialTargetsPreserveConfirmedDiffAndUnknownAbsence(t *testing.T) {
	for _, recursive := range []bool{false, true} {
		t.Run(fmt.Sprintf("recursive=%t", recursive), func(t *testing.T) {
			var renders atomic.Int32
			backend := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, a domain.Application, revision string) ([]string, error) {
				renders.Add(1)
				docs := []string{configMap(revision), appManifest(graphApp("child"))}
				name := "base-only"
				if revision == graphHead {
					name = "head-only"
				}
				docs = append(docs, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"`+name+`"}}`)
				return docs, &domain.RenderPartial{Cause: domain.Fail("render_partial", "render", "template app/templates/allowlist.yaml:6 uses unavailable function mustExampleIPs", "restore custom Helm context", nil)}
			}}
			runner := graphRunner(backend, "root")
			var report domain.Report
			if recursive {
				report = runner.RunRecursive(context.Background(), graphPair())
			} else {
				report = runner.Run(context.Background(), graphPair())
			}
			if report.ExitCode() != 3 || report.Status != "partial_failure" || !report.ChangesFound || len(report.Applications) != 1 || renders.Load() != 2 {
				t.Fatalf("unexpected partial report: %+v renders=%d", report, renders.Load())
			}
			result := findApp(t, report, "root")
			if result.Execution != "failed" || result.State != "changed" || len(result.Issues) != 2 || len(result.Resources) != 4 {
				t.Fatalf("lost partial resources: %+v", result)
			}
			for _, res := range result.Resources {
				switch res.ID.Name {
				case "settings":
					if res.State != "changed" {
						t.Fatal("confirmed change lost", res)
					}
				case "base-only", "head-only":
					if res.State != "unknown" || res.Comparison != "incomplete" || len(res.Changes) != 0 {
						t.Fatal("false addition/removal", res)
					}
				}
			}
			if recursive && (report.Graph.BaseComplete || report.Graph.HeadComplete || result.BaseTarget.Execution != "failed" || result.HeadTarget.Execution != "failed") {
				t.Fatal("partial graph claimed complete")
			}
			encoded, err := reporter.Encode(report, "json", security.Redactor{})
			if err != nil || !strings.Contains(string(encoded), "render_partial") {
				t.Fatal("JSON lost partial failure", err)
			}
		})
	}
}

// Application specs are compared as JSON, as rendered targets are: a number
// held as a Go int on one side and as a json.Number on the other is the same
// value, and only the changed field is reported.
func TestSpecNumbersCompareAsJSON(t *testing.T) {
	base, head := graphApp("app"), graphApp("app")
	base.Spec["revisionHistoryLimit"] = 3
	head.Spec["revisionHistoryLimit"] = json.Number("3")
	head.Spec["project"] = "other"
	node := func(app domain.Application) *graph.Node {
		return &graph.Node{Application: app, Root: app.ID, Path: []domain.ApplicationID{app.ID}}
	}
	got := compareNode(node(base), node(head), presence{}, security.Redactor{})
	if len(got.SpecChanges) != 1 || got.SpecChanges[0].Path != "/spec/project" || got.State != "changed" {
		t.Fatalf("spec changes = %+v, state %s; want only /spec/project", got.SpecChanges, got.State)
	}
}

type slowLookups struct {
	graphBackend
	active, peak *atomic.Int32
}

func (b slowLookups) Application(ctx context.Context, id domain.ApplicationID) (domain.Application, error) {
	now := b.active.Add(1)
	defer b.active.Add(-1)
	for old := b.peak.Load(); now > old && !b.peak.CompareAndSwap(old, now); old = b.peak.Load() {
	}
	time.Sleep(20 * time.Millisecond)
	return b.graphBackend.Application(ctx, id)
}

// Explicit roots are looked up concurrently within render.maxConcurrency; a
// failed lookup stays with its own root and every root keeps its order.
func TestRecursiveRootLookupsRunConcurrently(t *testing.T) {
	var active, peak atomic.Int32
	apps := map[string]domain.Application{}
	names := []string{"a", "b", "c", "d", "e", "f"}
	for _, name := range names {
		if name != "c" {
			apps[name] = graphApp(name)
		}
	}
	runner := graphRunner(graphBackend{}, names...)
	runner.Config.Render.MaxConcurrency = 3
	runner.Renderers["one"] = slowLookups{graphBackend: graphBackend{apps: apps, render: func(_ context.Context, _ domain.Application, rev string) ([]string, error) {
		return []string{configMap(rev)}, nil
	}}, active: &active, peak: &peak}
	report := runner.RunRecursive(context.Background(), graphPair())
	if got := peak.Load(); got < 2 || got > 3 {
		t.Fatalf("peak concurrent root lookups %d, want 2..3", got)
	}
	if len(report.Applications) != len(names) || report.Status != "partial_failure" {
		t.Fatalf("status %s, %d Applications", report.Status, len(report.Applications))
	}
	for i, app := range report.Applications {
		if app.ID.Name != names[i] || (app.Execution == "failed") != (app.ID.Name == "c") {
			t.Fatalf("Application %d: %s %s", i, app.ID.Name, app.Execution)
		}
	}
}
