package reposerver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"diffaro/internal/domain"
)

// invokeMu serializes fake repo-server calls that record into shared test
// state, because the two calls of a repeated render run concurrently.
var invokeMu sync.Mutex

// The two calls of a repeated render run at the same time, so the render
// timeout covers about one render; the first failure cancels the other call
// and is the one reported.
func TestRepeatedRenderCallsRunConcurrently(t *testing.T) {
	secret := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db"},"data":{"user":"YXBw"}}`
	t.Run("both calls in flight", func(t *testing.T) {
		snapshot, app, project := fixture()
		var arrived sync.WaitGroup
		arrived.Add(2)
		both := make(chan struct{})
		go func() { arrived.Wait(); close(both) }()
		client := &Client{API: fakeAPI(t, func(int) map[string]any { return project }), snapshot: snapshot, invoke: func(context.Context, map[string]any) (Response, error) {
			arrived.Done()
			select {
			case <-both:
				return Response{Revision: commit, SourceType: "Helm", Manifests: []string{secret}}, nil
			case <-time.After(5 * time.Second):
				return Response{}, errors.New("the repeated render waited for the first call to finish")
			}
		}}
		out, err := client.RenderDesired(context.Background(), app, commit)
		if err != nil || len(out) != 1 || out[0] != secret {
			t.Fatalf("concurrent calls did not render: %v %v", out, err)
		}
	})
	t.Run("first failure cancels the other call", func(t *testing.T) {
		snapshot, app, project := fixture()
		failure := domain.Fail(domain.CodeRepoServerFailed, "render", "fictional repo-server failure", "retry", nil)
		var calls atomic.Int32
		cancelled := make(chan struct{})
		client := &Client{API: fakeAPI(t, func(int) map[string]any { return project }), snapshot: snapshot, invoke: func(ctx context.Context, _ map[string]any) (Response, error) {
			if calls.Add(1) == 1 {
				return Response{}, failure
			}
			select {
			case <-ctx.Done():
				close(cancelled)
				return Response{}, ctx.Err()
			case <-time.After(5 * time.Second):
				return Response{Revision: commit, SourceType: "Helm", Manifests: []string{secret}}, nil
			}
		}}
		started := time.Now()
		_, err := client.RenderDesired(context.Background(), app, commit)
		if !errors.Is(err, failure) {
			t.Fatalf("the failing call's error was not reported: %v", err)
		}
		select {
		case <-cancelled:
		default:
			t.Fatal("the other call was not cancelled")
		}
		if time.Since(started) > 4*time.Second {
			t.Fatal("the failure waited for the other call")
		}
	})
}

// repo-server gives no template source to screen for randomness functions,
// so every Helm render is repeated: a field that differs between the two,
// such as a generated password, is marked and not compared, and renders
// that describe different resources are skipped.
func TestRepeatedRenderMarksFieldsThatDiffer(t *testing.T) {
	secret := func(name, password string) string {
		return `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"` + name + `"},"data":{"password":"` + password + `","user":"YXBw"}}`
	}
	for _, tc := range []struct {
		name   string
		render func(call int) []string
		check  func(t *testing.T, out []string, err error)
	}{
		{"identical renders", func(int) []string { return []string{secret("db", "c2FtZQ==")} }, func(t *testing.T, out []string, err error) {
			if err != nil || len(out) != 1 || out[0] != secret("db", "c2FtZQ==") {
				t.Fatalf("identical renders changed: %v %v", out, err)
			}
		}},
		{"generated password", func(call int) []string { return []string{secret("db", []string{"Zmlyc3Q=", "c2Vjb25k"}[call%2])} }, func(t *testing.T, out []string, err error) {
			if err != nil || len(out) != 1 || !strings.Contains(out[0], `"password":"`+domain.RenderUnstable+`"`) || !strings.Contains(out[0], `"user":"YXBw"`) {
				t.Fatalf("differing field not marked or equal field lost: %v %v", out, err)
			}
		}},
		{"random resource name", func(call int) []string { return []string{secret([]string{"db-a", "db-b"}[call%2], "c2FtZQ==")} }, func(t *testing.T, out []string, err error) {
			var skipped *domain.RenderSkipped
			if out != nil || !errors.As(err, &skipped) || !strings.Contains(skipped.Reason, "different resources for identical inputs") || issueCode(err) != domain.CodeRenderNondeterministic {
				t.Fatalf("different resources were not skipped: %v %v", out, err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, app, project := fixture()
			calls := 0
			client := &Client{API: fakeAPI(t, func(int) map[string]any { return project }), snapshot: snapshot, invoke: func(context.Context, map[string]any) (Response, error) {
				invokeMu.Lock()
				defer invokeMu.Unlock()
				calls++
				return Response{Revision: commit, SourceType: "Helm", Manifests: tc.render(calls - 1)}, nil
			}}
			out, err := client.RenderDesired(context.Background(), app, commit)
			if calls != 2 {
				t.Fatalf("render was not repeated: %d calls", calls)
			}
			tc.check(t, out, err)
		})
	}
}
