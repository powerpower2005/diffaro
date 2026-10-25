package localhelm

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
	"diffaro/internal/fetchcache"
	"diffaro/internal/gitsource"
	"diffaro/internal/httpx"
)

// countingGit serves fixed archives and counts reads per repository@sha.
// block, when set, holds every read until it is closed or the read's
// context ends; entered receives one value per read that started.
type countingGit struct {
	files   map[string]map[string][]byte
	block   chan struct{}
	entered chan string
	mu      sync.Mutex
	reads   map[string]int
}

func (g *countingGit) Archive(ctx context.Context, repo, sha string) (map[string][]byte, []string, error) {
	key := repo + "@" + sha
	g.mu.Lock()
	if g.reads == nil {
		g.reads = map[string]int{}
	}
	g.reads[key]++
	g.mu.Unlock()
	if g.entered != nil {
		g.entered <- key
	}
	if g.block != nil {
		select {
		case <-g.block:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	files := g.files[key]
	if files == nil {
		return nil, nil, renderError()
	}
	return files, nil, nil
}
func (g *countingGit) Resolve(context.Context, string, string) (string, error) { return testSHA, nil }
func (g *countingGit) count(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads[key]
}

func TestSharedSourceCacheReadsEachRevisionOnceAcrossInstances(t *testing.T) {
	key := "example/charts@" + testSHA
	git := &countingGit{files: map[string]map[string][]byte{key: {"app/Chart.yaml": []byte("name: app")}}}
	shared := gitsource.New(git, 1<<20)
	east, west := &Client{Git: git, Sources: shared}, &Client{Git: git, Sources: shared}
	for _, c := range []*Client{east, west, east} {
		if _, _, err := c.archive(context.Background(), "example/charts", testSHA); err != nil {
			t.Fatal(err)
		}
	}
	if git.count(key) != 1 {
		t.Fatalf("instances sharing one Git reader read the same revision %d times", git.count(key))
	}

	// Sparse reads share tree listings and blobs the same way.
	sparse := largeRepository()
	shared = gitsource.New(sparse, 64<<20)
	needs := &sourceNeeds{dirs: map[string]bool{"apps/api": true}, files: map[string]bool{}}
	for _, c := range []*Client{{Git: sparse, Sources: shared, SnapshotLimit: 64 << 20}, {Git: sparse, Sources: shared, SnapshotLimit: 64 << 20}} {
		if _, _, err := c.sourceFiles(context.Background(), "example/charts", testSHA, needs); err != nil {
			t.Fatal(err)
		}
	}
	for sha, count := range sparse.blobs {
		if count != 1 {
			t.Fatalf("blob %s read %d times across instances", sha, count)
		}
	}

	// A Client with another reader, such as another token or host, never
	// receives content from a cache filled through a different reader.
	other := &countingGit{files: git.files}
	isolated := &Client{Git: other, Sources: gitsource.New(git, 1<<20)}
	if _, _, err := isolated.archive(context.Background(), "example/charts", testSHA); err != nil || other.count(key) != 1 || git.count(key) != 1 {
		t.Fatalf("cache crossed Git readers: %v other=%d shared=%d", err, other.count(key), git.count(key))
	}
	if gitsource.New(fixtureGit{}, 0).ReadsWith(fixtureGit{}) {
		t.Fatal("uncomparable readers were treated as the same reader")
	}
}

func TestSharedSourceCacheKeepsPerInstanceSnapshotLimit(t *testing.T) {
	key := "example/charts@" + testSHA
	git := &countingGit{files: map[string]map[string][]byte{key: {"app/Chart.yaml": []byte(strings.Repeat("x", 40<<10))}}}
	shared := gitsource.New(git, 1<<20)
	large, small := &Client{Git: git, Sources: shared, SnapshotLimit: 64 << 10}, &Client{Git: git, Sources: shared, SnapshotLimit: 32 << 10}
	if _, _, err := large.archive(context.Background(), "example/charts", testSHA); err != nil {
		t.Fatal(err)
	}
	_, _, err := small.archive(context.Background(), "example/charts", testSHA)
	var failure *domain.Failure
	if !errors.As(err, &failure) || failure.Issue.Code != "limit_exceeded" || !strings.Contains(failure.Issue.Action, "render.maxSourceSnapshotBytes") {
		t.Fatal("cached content bypassed the instance snapshot limit", err)
	}
	// Reusing a revision already charged to an instance costs nothing more.
	if _, _, err := large.archive(context.Background(), "example/charts", testSHA); err != nil || large.bytes != 40<<10 || git.count(key) != 1 {
		t.Fatalf("reuse charged twice or refetched: %v bytes=%d reads=%d", err, large.bytes, git.count(key))
	}
}

func TestSharedSourceCacheStillAuthorizesEachInstance(t *testing.T) {
	req := fixtureRequest()
	app := req.Application
	files := req.Repositories[testRepo+"@"+testSHA]
	git := &countingGit{files: map[string]map[string][]byte{"example/charts@" + testSHA: files}}
	shared := gitsource.New(git, 1<<20)
	client := func(instance string, sourceRepos []any) (*Client, *bool) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/api/v1/projects/"):
				json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "demo"}, "spec": map[string]any{"sourceRepos": sourceRepos, "destinations": []any{map[string]any{"name": "*", "namespace": "*"}}}})
			case r.URL.Path == "/api/v1/settings":
				json.NewEncoder(w).Encode(req.Settings)
			case strings.HasPrefix(r.URL.Path, "/api/v1/clusters/"):
				json.NewEncoder(w).Encode(map[string]any{"server": "https://cluster.example.invalid", "name": "service-east", "info": map[string]any{"serverVersion": "1.31.0", "apiVersions": []string{"v1"}, "connectionState": map[string]any{"status": "Successful"}}})
			default:
				w.WriteHeader(404)
			}
		}))
		t.Cleanup(server.Close)
		executed := new(bool)
		return &Client{API: &argocd.Client{Instance: instance, HTTP: httpx.New(server.URL, "token-"+instance, time.Second, 1<<20)}, Git: git, Sources: shared, RepositoryURL: testRepo, Limit: 1 << 20, Execute: func(context.Context, Request) ([]string, error) { *executed = true; return nil, nil }}, executed
	}
	allowed, allowedRan := client("east", []any{"*"})
	if _, err := allowed.RenderDesired(context.Background(), app, testSHA); err != nil || !*allowedRan {
		t.Fatal("allowed instance did not render", err)
	}
	denied, deniedRan := client("west", []any{"https://github.com/example/other"})
	app.ID.Instance = "west"
	_, err := denied.RenderDesired(context.Background(), app, testSHA)
	var failure *domain.Failure
	if !errors.As(err, &failure) || failure.Issue.Code != "access_denied" || *deniedRan || denied.bytes != 0 {
		t.Fatalf("shared content bypassed the instance AppProject policy: %v executed=%t bytes=%d", err, *deniedRan, denied.bytes)
	}
}

func TestSourceReadsDoNotSerializeOnNetworkIO(t *testing.T) {
	git := &countingGit{files: map[string]map[string][]byte{"example/a@" + testSHA: {"a": []byte("a")}, "example/b@" + testSHA: {"b": []byte("b")}}, block: make(chan struct{}), entered: make(chan string, 2)}
	c := &Client{Git: git}
	errs := make(chan error, 2)
	for _, repo := range []string{"example/a", "example/b"} {
		go func() { _, _, err := c.archive(context.Background(), repo, testSHA); errs <- err }()
	}
	// Both downloads must be in flight at once; a held lock would admit one.
	for range 2 {
		select {
		case <-git.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("a download blocked another repository's download")
		}
	}
	close(git.block)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	// Public dependency downloads and Argo CD context reads are not serialized either.
	started, release := make(chan struct{}, 2), make(chan struct{})
	c.Download = func(ctx context.Context, location string, limit int64) ([]byte, error) {
		started <- struct{}{}
		<-release
		return []byte(location), nil
	}
	for _, location := range []string{"https://charts.example.invalid/a.tgz", "https://charts.example.invalid/b.tgz"} {
		go func() { _, err := c.download(context.Background(), location, 1<<10); errs <- err }()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("a dependency download blocked another")
		}
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestCancelledFirstReaderDoesNotFailOtherWaiters(t *testing.T) {
	key := "example/charts@" + testSHA
	git := &countingGit{files: map[string]map[string][]byte{key: {"app/Chart.yaml": []byte("name: app")}}, block: make(chan struct{}), entered: make(chan string, 4)}
	shared := gitsource.New(git, 1<<20)
	first, second := &Client{Git: git, Sources: shared}, &Client{Git: git, Sources: shared}
	ctx, cancel := context.WithCancel(context.Background())
	firstErr, secondErr := make(chan error, 1), make(chan error, 1)
	go func() { _, _, err := first.archive(ctx, "example/charts", testSHA); firstErr <- err }()
	<-git.entered
	go func() { _, _, err := second.archive(context.Background(), "example/charts", testSHA); secondErr <- err }()
	time.Sleep(20 * time.Millisecond) // let the second reader wait on the first fetch
	cancel()
	if err := <-firstErr; !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled reader did not stop", err)
	}
	// The waiter retries with its own context instead of inheriting the cancellation.
	select {
	case <-git.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not retry after the first reader was cancelled")
	}
	close(git.block)
	if err := <-secondErr; err != nil {
		t.Fatal("cancellation of one reader failed another", err)
	}
	if _, _, err := first.archive(context.Background(), "example/charts", testSHA); err != nil || git.count(key) != 2 {
		t.Fatalf("cancelled read was cached or the retried read was not: %v reads=%d", err, git.count(key))
	}
}

func TestTransientDependencyDownloadIsRetried(t *testing.T) {

	// A dependency download that failed transiently is fetched again.
	c := &Client{}
	downloads := 0
	c.Download = func(context.Context, string, int64) ([]byte, error) {
		downloads++
		if downloads == 1 {
			return nil, &fetchcache.TransientError{Err: skip("public Helm dependency download is unavailable")}
		}
		return []byte("chart"), nil
	}
	var skipped *domain.RenderSkipped
	if _, err := c.download(context.Background(), "https://charts.example.invalid/a.tgz", 1<<10); !errors.As(err, &skipped) {
		t.Fatal("transient download failure must still report its skip", err)
	}
	if data, err := c.download(context.Background(), "https://charts.example.invalid/a.tgz", 1<<10); err != nil || string(data) != "chart" || downloads != 2 {
		t.Fatal("transient download failure was cached", err, downloads)
	}
}

// deniedBlobGit denies blob reads in one repository; another repository holds
// the same content.
type deniedBlobGit struct{ *sparseGit }

func (g deniedBlobGit) Blob(ctx context.Context, repository, sha string) ([]byte, error) {
	if repository == "example/private" {
		return nil, domain.Fail("access_denied", "source", "denied", "allow Contents: read", nil)
	}
	return g.sparseGit.Blob(ctx, repository, sha)
}

func TestBlobFailureIsNotReplayedForAnotherRepository(t *testing.T) {
	git := deniedBlobGit{&sparseGit{files: map[string]string{"app/values.yaml": "replicas: 1"}}}
	cache := gitsource.New(git, 1<<20)
	sha := blobSHA("replicas: 1")
	var failure *domain.Failure
	if _, err := cache.Blob(context.Background(), "example/private", sha); !errors.As(err, &failure) || failure.Issue.Code != "access_denied" {
		t.Fatal("denied blob read accepted", err)
	}
	if data, err := cache.Blob(context.Background(), "example/charts", sha); err != nil || string(data) != "replicas: 1" {
		t.Fatal("one repository's blob failure was replayed for another", err)
	}
	if _, err := cache.Blob(context.Background(), "example/private", sha); !errors.As(err, &failure) {
		t.Fatal("another repository's content leaked into a denied read", err)
	}
}

// revisionRequest is fixtureRequest planned at sha with the same files, as
// the base and head of an Application whose chart did not change. edit
// changes the chart files and Helm source of both.
func revisionRequest(sha string, edit func(files map[string][]byte, helm map[string]any)) Request {
	req := fixtureRequest()
	files := maps.Clone(req.Repositories[testRepo+"@"+testSHA])
	helm := map[string]any{}
	edit(files, helm)
	req.Application.Spec["source"] = map[string]any{"repoURL": testRepo, "path": "app", "targetRevision": "main", "helm": helm}
	req.Plan[0].Revision = sha
	req.Plan[0].Source = map[string]any{"repoURL": testRepo, "path": "app", "targetRevision": sha, "helm": helm}
	req.Repositories = map[string]map[string][]byte{testRepo + "@" + sha: files}
	return req
}

// The bundled engine starts one worker for the base and head of an
// Application with identical render inputs; the head result, partial issues
// included, is what its own worker returns. Inputs that read the revision,
// and failures, are rendered on each side.
func TestBundledRenderOfIdenticalInputsStartsOneWorker(t *testing.T) {
	var workers atomic.Int32
	original := bundledProcess
	bundledProcess = func(ctx context.Context, req Request) ([]string, error) {
		workers.Add(1)
		return original(ctx, req)
	}
	defer func() { bundledProcess = original }()
	headSHA := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name    string
		edit    func(map[string][]byte, map[string]any)
		workers int32
	}{
		{"partial render", func(files map[string][]byte, _ map[string]any) {
			files["app/templates/custom.yaml"] = []byte(`{{ mustExampleIPs "demo" }}`)
		}, 1},
		{"revision parameter", func(_ map[string][]byte, helm map[string]any) {
			helm["parameters"] = []any{map[string]any{"name": "message", "value": "$ARGOCD_APP_REVISION"}}
		}, 2},
		{"failure", func(files map[string][]byte, _ map[string]any) {
			files["app/templates/fail.yaml"] = []byte(`{{ fail "stop" }}`)
		}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workers.Store(0)
			c := &Client{}
			base, head := revisionRequest(testSHA, tc.edit), revisionRequest(headSHA, tc.edit)
			var wg sync.WaitGroup
			var baseOut, headOut []string
			var baseErr, headErr error
			wg.Add(2)
			go func() { defer wg.Done(); baseOut, baseErr = c.execute(context.Background(), base) }()
			go func() { defer wg.Done(); headOut, headErr = c.execute(context.Background(), head) }()
			wg.Wait()
			if workers.Load() != tc.workers {
				t.Fatalf("base and head started %d workers, want %d", workers.Load(), tc.workers)
			}
			for _, side := range []struct {
				req Request
				out []string
				err error
			}{{base, baseOut, baseErr}, {head, headOut, headErr}} {
				want, wantErr := Process(context.Background(), side.req)
				if !reflect.DeepEqual(side.out, want) || !reflect.DeepEqual(domain.RenderIssues(side.err), domain.RenderIssues(wantErr)) || domain.IsRenderPartial(side.err) != domain.IsRenderPartial(wantErr) {
					t.Fatalf("revision %s: got %d targets, %v; its own worker returns %d targets, %v", side.req.Plan[0].Revision, len(side.out), side.err, len(want), wantErr)
				}
			}
		})
	}
}
