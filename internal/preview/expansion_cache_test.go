package preview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/graph"
	"diffaro/internal/manifest"
	"diffaro/internal/profile"
)

func TestExpansionCacheKeysAndTransientErrors(t *testing.T) {
	calls := 0
	var result error
	expand := func(context.Context, domain.Application, bool) (graph.Expansion, error) {
		calls++
		return graph.Expansion{Revision: "rendered"}, result
	}
	cache := newExpansionCache(domain.RetainedByteLimit, 1)
	base, head := cache.wrap(graphBase, expand), cache.wrap(graphHead, expand)
	app := graphApp("one")
	for range 2 {
		base(context.Background(), app, true)
	}
	if calls != 1 {
		t.Fatalf("identical input rendered %d times", calls)
	}
	changed := graphApp("one")
	changed.Source()["helm"] = map[string]any{"releaseName": "other"}
	base(context.Background(), changed, true)
	base(context.Background(), app, false)
	head(context.Background(), app, true)
	if calls != 4 {
		t.Fatalf("spec, role or revision shared a cached render: %d calls", calls)
	}

	// A deadline may succeed on retry; a backend failure is a final result.
	result = context.DeadlineExceeded
	timeout := graphApp("timeout")
	for range 2 {
		if _, err := base(context.Background(), timeout, true); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("deadline lost")
		}
	}
	if calls != 6 {
		t.Fatalf("deadline was cached: %d calls", calls)
	}
	result = domain.Fail("backend_error", "render", "failed", "retry", nil)
	failed := graphApp("failed")
	for range 2 {
		if _, err := base(context.Background(), failed, true); err != result {
			t.Fatal("cached failure changed")
		}
	}
	if calls != 7 {
		t.Fatalf("failure not reused: %d calls", calls)
	}
}

func TestExpansionCacheBudgetFallsBackToRendering(t *testing.T) {
	calls := 0
	expand := newExpansionCache(0, 1).wrap(graphBase, func(context.Context, domain.Application, bool) (graph.Expansion, error) {
		calls++
		return graph.Expansion{Revision: "rendered"}, nil
	})
	for range 2 {
		if exp, err := expand(context.Background(), graphApp("one"), true); err != nil || exp.Revision != "rendered" {
			t.Fatal("uncached render result lost")
		}
	}
	if calls != 2 {
		t.Fatalf("over-budget result was retained: %d calls", calls)
	}
}

// Candidate probes and the final walk share renders: every input renders once
// per revision and role, and the comparison is unchanged.
func TestDiscoveredRunRendersEachInputOnce(t *testing.T) {
	type call struct {
		name, revision string
		root           bool
	}
	calls := map[call]int{}
	var mu sync.Mutex
	backend := countingBackend{graphBackend: graphBackend{render: func(_ context.Context, app domain.Application, rev string) ([]string, error) {
		if app.ID.Name == "parent" {
			return []string{appManifest(graphApp("child"))}, nil
		}
		return []string{configMap(rev)}, nil
	}}, count: func(app domain.Application, rev string, root bool) {
		mu.Lock()
		calls[call{app.ID.Name, rev, root}]++
		mu.Unlock()
	}}
	runner := graphRunner(graphBackend{})
	runner.Renderers["one"] = backend
	runner.Config.Discovery = &profile.Discovery{Mode: "repository"}
	runner.Config.Instances = []profile.Instance{{ID: "one"}}
	inv := &fakeInventory{apps: []domain.OwnedApplication{{Application: graphApp("child")}, {Application: graphApp("parent")}}}
	report := runner.RunDiscovered(context.Background(), graphPair(), map[string]domain.ApplicationInventory{"one": inv})
	if report.Status != "success" || !report.ChangesFound || findApp(t, report, "child").State != "changed" || len(findApp(t, report, "child").HeadTarget.GraphPath) != 2 {
		t.Fatalf("unexpected report: %+v", report)
	}
	want := map[call]int{
		{"parent", graphBase, true}: 1, {"parent", graphHead, true}: 1,
		{"child", graphBase, false}: 1, {"child", graphHead, false}: 1,
		// The child's own candidate probe renders it as a root.
		{"child", graphBase, true}: 1, {"child", graphHead, true}: 1,
	}
	if len(calls) != len(want) {
		t.Fatalf("render calls %v", calls)
	}
	for k, n := range want {
		if calls[k] != n {
			t.Fatalf("render calls %v", calls)
		}
	}
}

type countingBackend struct {
	graphBackend
	count func(domain.Application, string, bool)
}

func (b countingBackend) Render(ctx context.Context, a domain.Application, rev string) ([]string, error) {
	b.count(a, rev, true)
	return b.graphBackend.Render(ctx, a, rev)
}
func (b countingBackend) RenderDesired(ctx context.Context, a domain.Application, rev string) ([]string, error) {
	b.count(a, rev, false)
	return b.graphBackend.RenderDesired(ctx, a, rev)
}

func TestExpansionCacheWaitsForInFlightRender(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	expand := newExpansionCache(domain.RetainedByteLimit, 1).wrap(graphBase, func(context.Context, domain.Application, bool) (graph.Expansion, error) {
		calls.Add(1)
		<-release
		return graph.Expansion{Revision: "rendered"}, nil
	})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if exp, _ := expand(context.Background(), graphApp("one"), true); exp.Revision != "rendered" {
				t.Error("waiter lost the leader's result")
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent identical input rendered %d times", calls.Load())
	}
}

// Candidate probes and both final walks use render.maxConcurrency; the
// report does not depend on scheduling.
func TestDiscoveredRunBoundsConcurrencyDeterministically(t *testing.T) {
	for _, limit := range []int{1, 3} {
		var active, peak atomic.Int32
		backend := graphBackend{render: func(_ context.Context, app domain.Application, rev string) ([]string, error) {
			now := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); now > old && !peak.CompareAndSwap(old, now); old = peak.Load() {
			}
			time.Sleep(5 * time.Millisecond)
			if app.ID.Name == "parent" {
				return []string{appManifest(graphApp("app-0"))}, nil
			}
			return []string{configMap(app.ID.Name + rev)}, nil
		}}
		var first []byte
		for run := range 3 {
			runner := graphRunner(backend)
			runner.Config.Render.MaxConcurrency = limit
			runner.Config.Discovery = &profile.Discovery{Mode: "repository"}
			runner.Config.Instances = []profile.Instance{{ID: "one"}}
			inv := &fakeInventory{apps: []domain.OwnedApplication{{Application: graphApp("parent")}}}
			for i := range 8 {
				inv.apps = append(inv.apps, domain.OwnedApplication{Application: graphApp(fmt.Sprintf("app-%d", i))})
			}
			report := runner.RunDiscovered(context.Background(), graphPair(), map[string]domain.ApplicationInventory{"one": inv})
			if report.Status != "success" || len(report.Applications) != 9 || len(findApp(t, report, "app-0").HeadTarget.GraphPath) != 2 {
				t.Fatalf("limit %d: unexpected report: %+v", limit, report)
			}
			encoded, _ := json.Marshal(report)
			if run == 0 {
				first = encoded
			} else if string(encoded) != string(first) {
				t.Fatalf("limit %d: report depends on scheduling", limit)
			}
		}
		if got := int(peak.Load()); got > limit || (limit > 1 && got < 2) {
			t.Fatalf("limit %d: peak concurrent renders %d", limit, got)
		}
	}
}

// A waiter whose own context ends stops waiting for another walk's render.
func TestExpansionCacheWaiterHonorsItsContext(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	expand := func(context.Context, domain.Application, bool) (graph.Expansion, error) {
		close(started)
		<-release
		return graph.Expansion{}, nil
	}
	cache := newExpansionCache(domain.RetainedByteLimit, 1)
	wrapped := cache.wrap(graphBase, expand)
	leader := make(chan struct{})
	go func() { defer close(leader); _, _ = wrapped(context.Background(), graphApp("slow"), true) }()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waited := make(chan error, 1)
	go func() { _, err := wrapped(ctx, graphApp("slow"), true); waited <- err }()
	select {
	case err := <-waited:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter ignored its cancelled context")
	}
	close(release)
	<-leader
}

// countedEncoding counts its JSON encodings.
type countedEncoding struct{ n *atomic.Int32 }

func (c countedEncoding) MarshalJSON() ([]byte, error) {
	c.n.Add(1)
	return []byte(`"value"`), nil
}

// Candidate probes and final walks reuse one render per revision. Its
// resources are encoded once, for the cache budget, and every walk charges
// those bytes, plus the spec, to its own budget: a budget one byte short fails.
func TestCachedExpansionIsEncodedOncePerRender(t *testing.T) {
	var encodings, renders atomic.Int32
	id := domain.ResourceID{Kind: "ConfigMap", Namespace: "apps", Name: "settings"}
	resources := map[string]manifest.Resource{id.String(): {ID: id, Object: map[string]any{"data": countedEncoding{&encodings}}}}
	expand := func(context.Context, domain.Application, bool) (graph.Expansion, error) {
		renders.Add(1)
		return graph.Expansion{Revision: "rendered", Resources: resources}, nil
	}
	root := graphApp("root")
	spec, _ := json.Marshal(root.Spec)
	encoded, _ := json.Marshal(resources)
	encodings.Store(0)
	cache := newExpansionCache(domain.RetainedByteLimit, 1)
	walk := func(revision string, budget int) graph.Snapshot {
		w := graph.Walker{MaxDepth: 1, MaxApplications: 1, MaxBytes: budget, Expand: cache.wrap(revision, expand)}
		return w.Walk(context.Background(), []domain.Application{root})
	}
	for _, revision := range []string{graphBase, graphHead, graphBase, graphHead} {
		if s := walk(revision, len(spec)+len(encoded)); !s.Complete || !s.Nodes[root.ID].Rendered {
			t.Fatalf("%s: a budget of the spec and resource bytes did not fit: %+v", revision, s.Faults)
		}
	}
	if renders.Load() != 2 || encodings.Load() != 2 {
		t.Fatalf("%d renders encoded %d times", renders.Load(), encodings.Load())
	}
	if s := walk(graphBase, len(spec)+len(encoded)-1); s.Complete || s.Nodes[root.ID].Rendered || !hasFaultCode(s, domain.CodeGraphLimitExceeded) {
		t.Fatal("a budget one byte short of the measured resources was accepted")
	}
}

func hasFaultCode(s graph.Snapshot, code string) bool {
	for _, fault := range s.Faults {
		var failure *domain.Failure
		if errors.As(fault.Err, &failure) && failure.Issue.Code == code {
			return true
		}
	}
	return false
}

// The cache budget counts what encoding the whole expansion would.
func TestExpansionSizeMatchesItsEncoding(t *testing.T) {
	id := domain.ResourceID{Kind: "ConfigMap", Namespace: "apps", Name: "a<b>&c"}
	for _, exp := range []graph.Expansion{
		{},
		{Resources: map[string]manifest.Resource{}},
		{Repository: graphRepo, Revision: graphHead, RevisionRule: "selected-repository-pair", RepeatedResources: []domain.ResourceID{id}, Sources: []domain.SourceSummary{{Position: 1, Repository: graphRepo}}, Resources: map[string]manifest.Resource{id.String(): {ID: id, Secret: true, Object: map[string]any{"data": map[string]any{"html": "<&>", "n": json.Number("1.50")}}}}},
	} {
		encoded, _ := json.Marshal(exp)
		exp.ResourceBytes = graph.ResourceSize(exp.Resources)
		if got := expansionSize(exp); got != len(encoded) {
			t.Fatalf("size %d, encoding %d bytes: %+v", got, len(encoded), exp)
		}
	}
	unencodable := graph.Expansion{Resources: map[string]manifest.Resource{"x": {Object: map[string]any{"bad": func() {}}}}}
	if unencodable.ResourceBytes = graph.ResourceSize(unencodable.Resources); expansionSize(unencodable) != -1 {
		t.Fatal("an unencodable expansion was given a size")
	}
}
