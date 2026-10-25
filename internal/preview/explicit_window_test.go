package preview

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

const windowRepo = "https://github.com/example/charts.git"

// windowBackend gives each explicit Application one of several outcomes and
// delays every call by a duration from delay.
type windowBackend struct {
	delay func(name, call string) time.Duration
	mu    sync.Mutex
	calls map[string]int
}

func (b *windowBackend) wait(ctx context.Context, name, call string) error {
	b.mu.Lock()
	if b.calls == nil {
		b.calls = map[string]int{}
	}
	b.calls[name+"/"+call]++
	b.mu.Unlock()
	if b.delay == nil {
		return nil
	}
	select {
	case <-time.After(b.delay(name, call)):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *windowBackend) count(name, call string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[name+"/"+call]
}

func (b *windowBackend) Application(ctx context.Context, id domain.ApplicationID) (domain.Application, error) {
	if err := b.wait(ctx, id.Name, "lookup"); err != nil {
		return domain.Application{}, err
	}
	repo := windowRepo
	if id.Name[len(id.Name)-1] == '3' {
		repo = "https://github.com/example/other.git"
	}
	return domain.Application{ID: id, Spec: map[string]any{"project": "demo-" + id.Name, "source": map[string]any{"repoURL": repo, "path": "chart", "helm": map[string]any{}}, "destination": map[string]any{"name": "cluster", "namespace": "demo"}}}, nil
}

func (b *windowBackend) CheckProject(ctx context.Context, app domain.Application) error {
	if err := b.wait(ctx, app.ID.Name, "project"); err != nil {
		return err
	}
	if app.ID.Name[len(app.ID.Name)-1] == '5' {
		return domain.Fail(domain.CodeAccessDenied, "permission", "project denied", "allow project read", nil)
	}
	return nil
}

func (b *windowBackend) Render(ctx context.Context, app domain.Application, revision string) ([]string, error) {
	if err := b.wait(ctx, app.ID.Name, "render-"+revision); err != nil {
		return nil, err
	}
	switch app.ID.Name[len(app.ID.Name)-1] {
	case '1':
		if revision == "base" {
			return nil, domain.Fail(domain.CodeRenderFailed, "render", "base template failed", "fix the chart", nil)
		}
	case '7':
		if revision == "head" {
			return nil, domain.Fail(domain.CodeRenderFailed, "render", "head template failed", "fix the chart", nil)
		}
	}
	return []string{fmt.Sprintf(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":%q},"data":{"revision":%q,"padding":"%0200d"}}`, app.ID.Name, revision, 0)}, nil
}

func windowConfig(apps, concurrency int) profile.Config {
	c := profile.Config{Git: profile.Git{RepositoryURL: windowRepo}, Render: profile.Render{Timeout: "10s", MaxConcurrency: concurrency, MaxResponseBytes: 1 << 20, MaxDocuments: 10}}
	for i := range apps {
		c.Applications = append(c.Applications, domain.ApplicationID{Instance: "one", Namespace: "argocd", Name: fmt.Sprintf("app-%02d", i)})
	}
	return c
}

func encodeReport(t *testing.T, report domain.Report) string {
	t.Helper()
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestExplicitWindowMatchesSequentialOrder renders explicit Applications with
// random per-call delays, so they complete in a different order on every
// run, and requires the report of a sequential run without delays: the same
// results and issues in the same order, the same retained prefix when the
// byte budget runs out, and the same status.
func TestExplicitWindowMatchesSequentialOrder(t *testing.T) {
	const apps = 16
	pair := domain.RevisionPair{Base: "base", Head: "head"}
	for _, budget := range []int{0, 3000} {
		sequential := Runner{Config: windowConfig(apps, 1), Renderers: map[string]domain.Renderer{"one": &windowBackend{}}, maxResultBytes: budget}.Run(context.Background(), pair)
		kept, dropped := 0, 0
		for _, app := range sequential.Applications {
			if len(app.Issues) > 0 && app.Issues[0].Code == domain.CodeReportLimitExceeded {
				dropped++
			} else {
				kept++
			}
		}
		if budget > 0 && (kept == 0 || dropped == 0) {
			t.Fatalf("budget %d must keep a prefix and drop the rest: kept %d, dropped %d", budget, kept, dropped)
		}
		if budget == 0 && (dropped != 0 || sequential.Status != domain.StatusPartialFailure) {
			t.Fatalf("the fixture must mix outcomes without a budget limit: status %s, dropped %d", sequential.Status, dropped)
		}
		want := encodeReport(t, sequential)
		for seed := int64(1); seed <= 8; seed++ {
			random := rand.New(rand.NewSource(seed))
			var mu sync.Mutex
			backend := &windowBackend{delay: func(string, string) time.Duration {
				mu.Lock()
				defer mu.Unlock()
				return time.Duration(random.Intn(8)) * time.Millisecond
			}}
			got := Runner{Config: windowConfig(apps, 4), Renderers: map[string]domain.Renderer{"one": backend}, maxResultBytes: budget}.Run(context.Background(), pair)
			if encodeReport(t, got) != want {
				t.Fatalf("budget %d seed %d: the concurrent report differs from the sequential report", budget, seed)
			}
		}
	}
}

// TestExplicitHeadRendersAlongsideBase requires base and head of one
// Application to render at the same time when a slot is free, and the head
// side to be dropped when the base render fails outright.
func TestExplicitHeadRendersAlongsideBase(t *testing.T) {
	const render = 150 * time.Millisecond
	backend := &windowBackend{delay: func(_ string, call string) time.Duration {
		if call == "render-base" || call == "render-head" {
			return render
		}
		return 0
	}}
	c := windowConfig(1, 2)
	start := time.Now()
	report := Runner{Config: c, Renderers: map[string]domain.Renderer{"one": backend}}.Run(context.Background(), domain.RevisionPair{Base: "base", Head: "head"})
	if elapsed := time.Since(start); report.Status != domain.StatusSuccess || elapsed >= 2*render {
		t.Fatalf("base and head must render concurrently with a free slot: status %s, %v", report.Status, elapsed)
	}
	failing := windowConfig(2, 2)
	failing.Applications = failing.Applications[1:] // app-01 fails at base
	report = Runner{Config: failing, Renderers: map[string]domain.Renderer{"one": &windowBackend{}}}.Run(context.Background(), domain.RevisionPair{Base: "base", Head: "head"})
	app := report.Applications[0]
	if app.Execution != domain.ExecutionFailed || len(app.Issues) != 1 || app.Issues[0].Revision != "base" || app.HeadSources != nil {
		t.Fatalf("a base failure must be the only result, as in a sequential run: %+v", app)
	}
}

// TestExplicitWindowStaysWithinConcurrency counts concurrent backend calls,
// including head renders alongside base renders.
func TestExplicitWindowStaysWithinConcurrency(t *testing.T) {
	var mu sync.Mutex
	active, peak := 0, 0
	backend := &concurrencyProbe{enter: func() func() {
		mu.Lock()
		active++
		peak = max(peak, active)
		mu.Unlock()
		return func() { mu.Lock(); active--; mu.Unlock() }
	}}
	for _, limit := range []int{1, 3} {
		peak = 0
		report := Runner{Config: windowConfig(7, limit), Renderers: map[string]domain.Renderer{"one": backend}}.Run(context.Background(), domain.RevisionPair{Base: "base", Head: "head"})
		if len(report.Applications) != 7 || peak > limit {
			t.Fatalf("limit %d: peak %d concurrent calls", limit, peak)
		}
	}
}

type concurrencyProbe struct{ enter func() func() }

func (p *concurrencyProbe) Application(_ context.Context, id domain.ApplicationID) (domain.Application, error) {
	defer p.enter()()
	time.Sleep(time.Millisecond)
	return domain.Application{ID: id, Spec: map[string]any{"project": "demo", "source": map[string]any{"repoURL": windowRepo, "path": "chart", "helm": map[string]any{}}, "destination": map[string]any{"name": "cluster", "namespace": "demo"}}}, nil
}

func (p *concurrencyProbe) Render(_ context.Context, _ domain.Application, _ string) ([]string, error) {
	defer p.enter()()
	time.Sleep(3 * time.Millisecond)
	return []string{}, nil
}

// TestExplicitTimeoutBoundsEachStage requires render.timeout to bound each
// stage of an explicit Application, as for a graph node, not the whole
// comparison: a lookup, project check and renders that each finish within
// the timeout, but not together, complete.
func TestExplicitTimeoutBoundsEachStage(t *testing.T) {
	backend := &windowBackend{delay: func(_ string, call string) time.Duration {
		switch call {
		case "lookup", "render-base", "render-head":
			return 500 * time.Millisecond
		case "project":
			return 100 * time.Millisecond
		}
		return 0
	}}
	c := windowConfig(1, 1)
	c.Render.Timeout = "800ms"
	report := Runner{Config: c, Renderers: map[string]domain.Renderer{"one": backend}}.Run(context.Background(), domain.RevisionPair{Base: "base", Head: "head"})
	if app := report.Applications[0]; report.Status != domain.StatusSuccess || app.Execution != domain.ExecutionComplete || app.State != domain.StateChanged {
		t.Fatalf("stages within the timeout must complete: %s %+v", report.Status, app.Issues)
	}
	if backend.count("app-00", "project") != 1 || backend.count("app-00", "render-head") != 1 {
		t.Fatal("the project check and head render must each run once")
	}
}
