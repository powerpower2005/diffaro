package preview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

// panickingBackend panics for one Application, at one lookup or render
// call, and renders every other one.
type panickingBackend struct {
	limitedBackend
	name, at, revision string
}

//go:noinline
func (b *panickingBackend) fault() { panic(fmt.Sprintf("fictional-secret-value in %s", b.name)) }

func (b *panickingBackend) Application(ctx context.Context, id domain.ApplicationID) (domain.Application, error) {
	if id.Name == b.name && b.at == "lookup" {
		b.fault()
	}
	app, err := b.limitedBackend.Application(ctx, id)
	if id.Name == b.name && b.at == "plan" {
		// A second source from another repository makes the plan resolve it.
		app.Spec["sources"] = []any{app.Source(), map[string]any{"repoURL": "https://github.com/example/other.git", "targetRevision": "main", "ref": "values"}}
		delete(app.Spec, "source")
	}
	return app, err
}

func (b *panickingBackend) CheckProject(_ context.Context, app domain.Application) error {
	if app.ID.Name == b.name && b.at == "project" {
		b.fault()
	}
	return nil
}

func (b *panickingBackend) ResolveSourceRevision(context.Context, domain.Application, string, string) (string, error) {
	b.fault()
	return "", nil
}

func (b *panickingBackend) Render(ctx context.Context, app domain.Application, revision string) ([]string, error) {
	if app.ID.Name == b.name && b.at == "render" && revision == b.revision {
		b.fault()
	}
	return b.limitedBackend.Render(ctx, app, revision)
}

// assertPanicIssue checks that issue reports a panic of the fake backend as
// a Diffaro bug with its stack, and withholds the panic value.
func assertPanicIssue(t *testing.T, issue domain.Issue) {
	t.Helper()
	if issue.Code != domain.CodeInternalError || issue.ReasonCode != domain.ReasonPanic || issue.Diagnostic == nil {
		t.Fatalf("issue = %+v", issue)
	}
	if strings.Contains(issue.Reason, "fictional-secret-value") {
		t.Fatalf("panic value leaked: %q", issue.Reason)
	}
	if len(issue.Diagnostic.Stack) == 0 || !strings.HasPrefix(issue.Diagnostic.Stack[0], "diffaro/internal/preview.(*panickingBackend).fault ") {
		t.Fatalf("stack must start at the panicking frame: %v", issue.Diagnostic.Stack)
	}
}

// A panic while one Application is looked up or rendered on either side,
// also on the head side's own goroutine, fails that Application only: the
// others keep their results and the run is a partial failure.
func TestPanicFailsOnlyItsApplication(t *testing.T) {
	for _, tc := range []struct{ at, revision, stage string }{{"lookup", "", "preview"}, {"plan", "", "revision"}, {"project", "", "permission"}, {"render", graphBase, "render"}, {"render", graphHead, "render"}} {
		t.Run(tc.at+" "+tc.revision[:min(len(tc.revision), 4)], func(t *testing.T) {
			c := profile.Config{Git: profile.Git{RepositoryURL: "https://github.com/example/charts.git"}, Render: profile.Render{Timeout: "5s", MaxConcurrency: 4, MaxResponseBytes: 1 << 20, MaxDocuments: 10}}
			for _, name := range []string{"a", "b", "c"} {
				c.Applications = append(c.Applications, domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: name})
			}
			b := &panickingBackend{name: "b", at: tc.at, revision: tc.revision}
			report := Runner{Config: c, Renderers: map[string]domain.Renderer{"test": b}}.Run(context.Background(), domain.RevisionPair{Base: graphBase, Head: graphHead})
			if len(report.Applications) != 3 || report.Status != domain.StatusPartialFailure {
				t.Fatalf("status=%s apps=%d", report.Status, len(report.Applications))
			}
			for _, app := range report.Applications {
				if app.ID.Name != "b" {
					if app.Execution != domain.ExecutionComplete || len(app.Resources) != 1 {
						t.Fatalf("%s lost its result: %+v", app.ID.Name, app)
					}
					continue
				}
				if app.Execution != domain.ExecutionFailed || len(app.Issues) != 1 {
					t.Fatalf("panicking Application: %+v", app)
				}
				assertPanicIssue(t, app.Issues[0])
				// A panic in a step fails that step, like any error it returns:
				// a render panic names its stage and revision.
				if app.Issues[0].Stage != tc.stage || (tc.at == "render" && app.Issues[0].Revision != tc.revision) {
					t.Fatalf("panic is not scoped to its step: %+v", app.Issues[0])
				}
			}
		})
	}
}

// In a recursive walk a panicking child render fails that child; its
// siblings and parent are still compared.
func TestPanicFailsOnlyItsGraphNode(t *testing.T) {
	b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, a domain.Application, revision string) ([]string, error) {
		switch a.ID.Name {
		case "root":
			return []string{appManifest(graphApp("left")), appManifest(graphApp("right"))}, nil
		case "left":
			var settings map[string]string
			settings["fictional"] = "value"
		}
		return []string{configMap(revision)}, nil
	}}
	report := graphRunner(b, "root").RunRecursive(context.Background(), graphPair())
	if report.Status != domain.StatusPartialFailure {
		t.Fatalf("status = %s", report.Status)
	}
	if right := findApp(t, report, "right"); right.Execution != domain.ExecutionComplete || right.State != domain.StateChanged {
		t.Fatalf("sibling lost its result: %+v", right)
	}
	left := findApp(t, report, "left")
	if left.Execution != domain.ExecutionFailed {
		t.Fatalf("panicking child: %+v", left)
	}
	var found bool
	for _, issue := range left.Issues {
		if issue.ReasonCode == domain.ReasonPanic && strings.Contains(issue.Reason, "assignment to entry in nil map") && strings.Contains(issue.Diagnostic.Stack[0], "TestPanicFailsOnlyItsGraphNode") {
			found = true
		}
	}
	if !found {
		t.Fatalf("panic issue with the runtime message and stack missing: %+v", left.Issues)
	}
}

// When the call that leads a shared render panics, a waiter for the same key
// runs the render itself instead of waiting forever.
func TestFlightWaiterRecoversFromLeaderPanic(t *testing.T) {
	var f flight[string, int]
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		defer func() { _ = recover() }()
		f.do(context.Background(), "key", func() int {
			close(started)
			<-release
			panic("leader failed")
		}, func(int) bool { return true })
	}()
	<-started
	result := make(chan int, 1)
	go func() {
		v, _, _ := f.do(context.Background(), "key", func() int { return 7 }, func(int) bool { return true })
		result <- v
	}()
	close(release)
	select {
	case v := <-result:
		if v != 7 {
			t.Fatalf("waiter got %d", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter is stuck on the panicked call")
	}
}

// bounded lets every call finish and raises a worker's panic in its caller
// with the worker's stack.
func TestBoundedRaisesWorkerPanicInCaller(t *testing.T) {
	var mu sync.Mutex
	ran := map[int]bool{}
	var raised any
	func() {
		defer func() { raised = recover() }()
		bounded(4, 2, func(i int) {
			mu.Lock()
			ran[i] = true
			mu.Unlock()
			if i == 1 {
				var items []int
				_ = items[i]
			}
		})
	}()
	var failure *domain.Failure
	if err, ok := raised.(error); !ok || !errors.As(err, &failure) || failure.Issue.ReasonCode != domain.ReasonPanic {
		t.Fatalf("raised = %v", raised)
	}
	if !strings.Contains(failure.Issue.Diagnostic.Stack[0], "TestBoundedRaisesWorkerPanicInCaller") {
		t.Fatalf("worker stack lost: %v", failure.Issue.Diagnostic.Stack)
	}
	if len(ran) != 4 {
		t.Fatalf("calls ran: %v", ran)
	}
	// An owner boundary keeps the worker's failure rather than its own stack.
	if again := domain.PanicFailure(raised, "preview"); again != failure {
		t.Fatal("a re-raised panic lost its original failure")
	}
}

// alongside raises the panic of its goroutine in the caller that waits.
func TestAlongsideRaisesPanicInWaiter(t *testing.T) {
	slots := make(chan struct{}, 1)
	wait := alongside(slots, func() {
		var settings map[string]string
		settings["fictional"] = "value"
	})
	if wait == nil {
		t.Fatal("a free slot did not start the goroutine")
	}
	var raised any
	func() {
		defer func() { raised = recover() }()
		wait()
	}()
	var failure *domain.Failure
	if err, ok := raised.(error); !ok || !errors.As(err, &failure) || !strings.Contains(failure.Issue.Diagnostic.Stack[0], "TestAlongsideRaisesPanicInWaiter") {
		t.Fatalf("raised = %v", raised)
	}
	if len(slots) != 0 {
		t.Fatal("the panicking goroutine kept its slot")
	}
}

type panickingResolver struct{ graphBackend }

func (panickingResolver) ResolveSourceRevision(context.Context, domain.Application, string, string) (string, error) {
	var refs []string
	return refs[0], nil
}

// A panic outside the render step, here while a child's source commit is
// resolved, still fails only that node.
func TestPanicOutsideRenderFailsOnlyItsGraphNode(t *testing.T) {
	other := graphApp("other")
	other.Source()["repoURL"] = "https://github.com/example/other.git"
	backend := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, a domain.Application, revision string) ([]string, error) {
		if a.ID.Name == "root" {
			return []string{appManifest(other), appManifest(graphApp("sibling"))}, nil
		}
		return []string{configMap(revision)}, nil
	}}
	r := graphRunner(backend, "root")
	r.Renderers["one"] = panickingResolver{backend}
	report := r.RunRecursive(context.Background(), graphPair())
	if sibling := findApp(t, report, "sibling"); sibling.Execution != domain.ExecutionComplete {
		t.Fatalf("sibling lost its result: %+v", sibling)
	}
	failed := findApp(t, report, "other")
	if failed.Execution != domain.ExecutionFailed || len(failed.Issues) == 0 || failed.Issues[0].ReasonCode != domain.ReasonPanic || !strings.Contains(failed.Issues[0].Diagnostic.Stack[0], "panickingResolver") {
		t.Fatalf("resolver panic: %+v", failed)
	}
}
