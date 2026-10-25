package preview

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"diffaro/internal/domain"
)

type blockingRevisionBackend struct {
	domain.Renderer
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingRevisionBackend) ResolveSourceRevision(ctx context.Context, _ domain.Application, _, _ string) (string, error) {
	if b.calls.Add(1) == 1 {
		close(b.started)
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-b.release:
		return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
	}
}

func TestSourceRevisionWaitCancellationAndScopeIsolation(t *testing.T) {
	backend := &blockingRevisionBackend{started: make(chan struct{}), release: make(chan struct{})}
	runner := Runner{Renderers: map[string]domain.Renderer{"one": backend, "two": backend}}.newRun(domain.RevisionPair{})
	app := domain.Application{ID: domain.ApplicationID{Instance: "one", Namespace: "argocd", Name: "shop"}, Spec: map[string]any{"project": "demo", "destination": map[string]any{"name": "cluster"}}}
	resolve := runner.sourceResolver(app)
	done := make(chan error, 1)
	go func() { _, err := resolve(context.Background(), "https://example.com/chart", "main"); done <- err }()
	<-backend.started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolve(ctx, "https://example.com/chart", "main"); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting caller not cancelled: %v", err)
	}
	close(backend.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(context.Background(), "https://example.com/chart", "main"); err != nil || backend.calls.Load() != 1 {
		t.Fatal("waiter cancellation invalidated the original frozen result")
	}
	for _, change := range []func(*domain.Application){
		func(a *domain.Application) { a.ID.Instance = "two" },
		func(a *domain.Application) { a.Spec["project"] = "other" },
		func(a *domain.Application) { a.Spec["destination"] = map[string]any{"name": "other"} },
		func(a *domain.Application) { a.ID.Name = "other" },
	} {
		copy := domain.Application{ID: app.ID, Spec: domain.CloneSpec(app.Spec)}
		change(&copy)
		if _, err := runner.sourceResolver(copy)(context.Background(), "https://example.com/chart", "main"); err != nil {
			t.Fatal(err)
		}
	}
	if backend.calls.Load() != 5 {
		t.Fatal("revision cache crossed authorization scopes")
	}
}

// A caller whose context ends (for example a cancelled prefetch) must not
// leave its cancellation as the run's frozen resolution.
func TestSourceRevisionCancellationIsNotFrozen(t *testing.T) {
	backend := &blockingRevisionBackend{started: make(chan struct{}), release: make(chan struct{})}
	runner := Runner{Renderers: map[string]domain.Renderer{"one": backend}}.newRun(domain.RevisionPair{})
	app := domain.Application{ID: domain.ApplicationID{Instance: "one", Namespace: "argocd", Name: "shop"}, Spec: map[string]any{"project": "demo", "destination": map[string]any{"name": "cluster"}}}
	resolve := runner.sourceResolver(app)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := resolve(ctx, "https://example.com/chart", "main"); done <- err }()
	<-backend.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader not cancelled: %v", err)
	}
	close(backend.release)
	if sha, err := resolve(context.Background(), "https://example.com/chart", "main"); err != nil || sha == "" || backend.calls.Load() != 2 {
		t.Fatalf("cancellation replayed to a healthy caller: sha=%q err=%v calls=%d", sha, err, backend.calls.Load())
	}
}
