package localhelm

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
	"diffaro/internal/github"
)

// countingResolver counts provider ref lookups; block, when set, holds each
// lookup until released or the caller is cancelled.
type countingResolver struct {
	sparseGit
	calls atomic.Int32
	block chan struct{}
}

func (g *countingResolver) Resolve(ctx context.Context, _, _ string) (string, error) {
	g.calls.Add(1)
	if g.block != nil {
		select {
		case <-g.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return testSHA, nil
}

func resolverApp(instance, name string) domain.Application {
	return domain.Application{ID: domain.ApplicationID{Instance: instance, Namespace: "argocd", Name: name}}
}

func TestSourceRevisionLookupIsSharedAcrossApplications(t *testing.T) {
	git := &countingResolver{}
	c := &Client{API: &argocd.Client{Instance: "east"}, Git: git, RepositoryURL: testRepo}
	var wg sync.WaitGroup
	for _, name := range []string{"api", "web", "worker", "api", "web"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The same repository under another URL spelling maps to one key.
			if sha, err := c.ResolveSourceRevision(context.Background(), resolverApp("east", name), testRepo+".git", "main"); err != nil || sha != testSHA {
				t.Errorf("resolve: %q %v", sha, err)
			}
		}()
	}
	wg.Wait()
	if _, err := c.ResolveSourceRevision(context.Background(), resolverApp("east", "api"), testRepo, "main"); err != nil {
		t.Fatal(err)
	}
	if got := git.calls.Load(); got != 1 {
		t.Fatalf("repo@ref resolved %d times, want 1", got)
	}
	if _, err := c.ResolveSourceRevision(context.Background(), resolverApp("east", "api"), testRepo, "release"); err != nil || git.calls.Load() != 2 {
		t.Fatalf("a different ref must be resolved separately: calls=%d err=%v", git.calls.Load(), err)
	}
	// Instance routing is still checked per Application, before the cache.
	var failure *domain.Failure
	if _, err := c.ResolveSourceRevision(context.Background(), resolverApp("west", "api"), testRepo, "main"); !errors.As(err, &failure) || failure.Issue.Code != "routing_invalid" {
		t.Fatalf("cross-instance resolution served from cache: %v", err)
	}
	if _, err := c.ResolveSourceRevision(context.Background(), resolverApp("east", "api"), "https://github.com/example/values", "main"); err != nil || git.calls.Load() != 3 {
		t.Fatalf("another repository at the same ref must be resolved separately: calls=%d err=%v", git.calls.Load(), err)
	}
	if _, err := c.ResolveSourceRevision(context.Background(), resolverApp("east", "api"), "https://git.example.net/example/charts", "main"); err == nil || git.calls.Load() != 3 {
		t.Fatalf("repository outside the configured host resolved: %v", err)
	}
}

func TestCancelledSourceRevisionLookupIsNotShared(t *testing.T) {
	git := &countingResolver{block: make(chan struct{})}
	c := &Client{API: &argocd.Client{Instance: "east"}, Git: git, RepositoryURL: testRepo}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := c.ResolveSourceRevision(ctx, resolverApp("east", "api"), testRepo, "main")
		first <- err
	}()
	for git.calls.Load() == 0 {
		runtime.Gosched()
	}
	waiter := make(chan error, 1)
	go func() {
		_, err := c.ResolveSourceRevision(context.Background(), resolverApp("east", "web"), testRepo, "main")
		waiter <- err
	}()
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller: %v", err)
	}
	close(git.block)
	if err := <-waiter; err != nil {
		t.Fatalf("a waiter inherited another caller's cancellation: %v", err)
	}
	if sha, err := c.ResolveSourceRevision(context.Background(), resolverApp("east", "api"), testRepo, "main"); err != nil || sha != testSHA {
		t.Fatalf("cancellation was cached: %q %v", sha, err)
	}
}

// blockingBlobs holds every blob read until released so concurrent reads
// overlap, and fails the first read with the caller's cancellation.
type blockingBlobs struct {
	sparseGit
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (g *blockingBlobs) Blob(ctx context.Context, repository, sha string) ([]byte, error) {
	if g.calls.Add(1) == 1 {
		close(g.started)
	}
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.sparseGit.Blob(ctx, repository, sha)
}

func TestConcurrentBlobReadsShareOneRequest(t *testing.T) {
	git := &blockingBlobs{sparseGit: sparseGit{files: map[string]string{"app/values.yaml": "replicas: 1"}}, started: make(chan struct{}), release: make(chan struct{})}
	c := &Client{Git: git, SnapshotLimit: 1 << 20}
	entry := github.TreeEntry{Path: "app/values.yaml", SHA: blobSHA("replicas: 1"), Size: 11}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if data, err := c.blob(context.Background(), "example/charts", entry); err != nil || string(data) != "replicas: 1" {
				t.Errorf("blob: %q %v", data, err)
			}
		}()
	}
	<-git.started
	close(git.release)
	wg.Wait()
	if got := git.calls.Load(); got != 1 || c.bytes != 11 {
		t.Fatalf("blob fetched %d times, snapshot bytes %d", got, c.bytes)
	}
}

func TestCancelledBlobReadIsRetriedByWaiters(t *testing.T) {
	git := &blockingBlobs{sparseGit: sparseGit{files: map[string]string{"app/values.yaml": "replicas: 1"}}, started: make(chan struct{}), release: make(chan struct{})}
	c := &Client{Git: git, SnapshotLimit: 1 << 20}
	entry := github.TreeEntry{Path: "app/values.yaml", SHA: blobSHA("replicas: 1"), Size: 11}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := c.blob(ctx, "example/charts", entry); first <- err }()
	<-git.started
	waiter := make(chan error, 1)
	go func() { _, err := c.blob(context.Background(), "example/charts", entry); waiter <- err }()
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller: %v", err)
	}
	close(git.release)
	if err := <-waiter; err != nil {
		t.Fatalf("a waiter inherited another caller's cancellation: %v", err)
	}
	if git.calls.Load() != 2 || c.bytes != 11 {
		t.Fatalf("calls=%d bytes=%d", git.calls.Load(), c.bytes)
	}
}

type rateLimitedTree struct{ sparseGit }

func (g *rateLimitedTree) Tree(context.Context, string, string) ([]github.TreeEntry, error) {
	return nil, domain.Fail("rate_limited", "connection", "HTTP 403 Forbidden; API rate limit or secondary rate limit was reached", "wait for the API quota reset", nil)
}

func TestRateLimitedTreeDoesNotFallBackToArchive(t *testing.T) {
	git := &rateLimitedTree{}
	c := &Client{Git: git}
	_, _, err := c.sourceFiles(context.Background(), "example/charts", testSHA, &sourceNeeds{dirs: map[string]bool{"app": true}, files: map[string]bool{}})
	var failure *domain.Failure
	if !errors.As(err, &failure) || failure.Issue.Code != "rate_limited" || git.archives != 0 {
		t.Fatalf("rate limit must be reported, not retried as an archive: %v archives=%d", err, git.archives)
	}
}

// panickingBlobs panics on every blob read until healed; the first read
// waits for release before it panics, so a second read can wait for it.
type panickingBlobs struct {
	sparseGit
	calls   atomic.Int32
	healed  atomic.Bool
	started chan struct{}
	release chan struct{}
}

func (g *panickingBlobs) Blob(ctx context.Context, repository, sha string) ([]byte, error) {
	if g.healed.Load() {
		return g.sparseGit.Blob(ctx, repository, sha)
	}
	if g.calls.Add(1) == 1 {
		close(g.started)
		<-g.release
	}
	var parts [][]byte
	return parts[1], nil
}

// A panicking shared fetch fails every caller, including one waiting for
// it, with a panic failure instead of an empty blob, and is not kept: a
// later read fetches again.
func TestPanickingBlobFetchFailsItsWaitersAndIsNotKept(t *testing.T) {
	git := &panickingBlobs{sparseGit: sparseGit{files: map[string]string{"app/values.yaml": "replicas: 1"}}, started: make(chan struct{}), release: make(chan struct{})}
	c := &Client{Git: git, SnapshotLimit: 1 << 20}
	entry := github.TreeEntry{Path: "app/values.yaml", SHA: blobSHA("replicas: 1"), Size: 11}
	errs := make(chan error, 2)
	read := func() {
		data, err := c.blob(context.Background(), "example/charts", entry)
		if err == nil {
			err = errors.New("empty result " + string(data))
		}
		errs <- err
	}
	go read()
	<-git.started
	go read()
	runtime.Gosched()
	close(git.release)
	for range 2 {
		var failure *domain.Failure
		if err := <-errs; !errors.As(err, &failure) || failure.Issue.ReasonCode != domain.ReasonPanic {
			t.Fatalf("a caller of the panicked fetch got %v", err)
		}
	}
	git.healed.Store(true)
	if data, err := c.blob(context.Background(), "example/charts", entry); err != nil || string(data) != "replicas: 1" {
		t.Fatalf("the panicked fetch was kept: %q %v", data, err)
	}
}
