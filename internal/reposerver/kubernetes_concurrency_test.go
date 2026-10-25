package reposerver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

// A destination capability read does not block reads of other destinations
// or the tracking method; each read result is cached as before.
func TestDestinationCapabilityReadsRunConcurrently(t *testing.T) {
	_, app, _ := fixture()
	entered := make(chan string, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	k := &kubernetesContext{settings: map[string]any{}, version: "v1.31.0", apis: []any{"v1"}}
	k.capabilities = func(_ context.Context, server, name string) (argocd.ClusterCapabilities, error) {
		calls.Add(1)
		entered <- name
		<-release
		return argocd.ClusterCapabilities{Server: "https://" + name + ".example", Name: name, Version: "v1.32.0", APIVersions: []string{"v1"}}, nil
	}
	k.tracking = func(context.Context) (string, error) { return "annotation", nil }
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, name := range []string{"east", "west"} {
		dest := domain.CloneSpec(app.Spec)
		dest["destination"] = map[string]any{"name": name, "namespace": "demo"}
		other := domain.Application{ID: app.ID, Spec: dest}
		wg.Go(func() { _, results[i] = k.binding(context.Background(), other) })
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("capability reads of different destinations were serialized")
		}
	}
	tracked := make(chan error, 1)
	go func() { _, err := k.effectiveTracking(context.Background()); tracked <- err }()
	select {
	case err := <-tracked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tracking lookup waited for a capability read")
	}
	close(release)
	wg.Wait()
	if results[0] != nil || results[1] != nil || calls.Load() != 2 || len(k.clusters) != 2 {
		t.Fatal("concurrent capability reads were not cached", results, calls.Load())
	}
}

// Concurrent lookups of one destination share a successful read. A failed
// read is not cached: every waiter then reads again itself, exactly as when
// lookups ran one after another.
func TestDestinationCapabilityReadSharedOnlyOnSuccess(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("fail=", fail), func(t *testing.T) {
			k := &kubernetesContext{}
			var calls atomic.Int32
			started, release := make(chan struct{}), make(chan struct{})
			k.capabilities = func(context.Context, string, string) (argocd.ClusterCapabilities, error) {
				if calls.Add(1) == 1 {
					close(started)
					<-release
				}
				if fail {
					return argocd.ClusterCapabilities{}, errors.New("denied")
				}
				return argocd.ClusterCapabilities{Server: "https://remote.example", Version: "v1.32.0"}, nil
			}
			dest := map[string]any{"server": "https://remote.example"}
			const callers = 5
			errs := make(chan error, callers)
			go func() { _, err := k.destinationCapabilities(context.Background(), dest); errs <- err }()
			<-started
			for range callers - 1 {
				go func() { _, err := k.destinationCapabilities(context.Background(), dest); errs <- err }()
			}
			time.Sleep(50 * time.Millisecond)
			close(release)
			for range callers {
				if err := <-errs; (err != nil) != fail {
					t.Fatal(err)
				}
			}
			want := int32(1)
			if fail {
				want = callers
			}
			if calls.Load() != want || len(k.fetching) != 0 {
				t.Fatalf("reads=%d running=%d", calls.Load(), len(k.fetching))
			}
		})
	}
}

// Discovery reads run with bounded concurrency, but the capabilities and the
// reported failure are those of reading the groups in sorted order: an
// earlier group's failure wins over a later group that fails first.
func TestDiscoveryKeepsSortedResultsAndFirstFailure(t *testing.T) {
	collect := func(failing map[string]bool) ([]any, error, int32) {
		k := newKubernetesContext(profile.KubernetesContext{Namespace: "argocd"})
		var running, peak atomic.Int32
		k.read = func(_ context.Context, args ...string) (map[string]any, error) {
			p := args[len(args)-1]
			switch p {
			case "/version":
				return map[string]any{"gitVersion": "v1.31.0"}, nil
			case "/api":
				return map[string]any{"versions": []any{"v1"}}, nil
			case "/apis":
				groups := []any{}
				for i := 0; i < 40; i++ {
					groups = append(groups, map[string]any{"versions": []any{map[string]any{"groupVersion": fmt.Sprintf("g%02d.example.com/v1", i)}}})
				}
				return map[string]any{"groups": groups}, nil
			}
			n := running.Add(1)
			defer running.Add(-1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			if failing[p] {
				if p == "/apis/g03.example.com/v1" {
					time.Sleep(20 * time.Millisecond)
					return map[string]any{"resources": "invalid"}, nil
				}
				return nil, errors.New("read failed " + p)
			}
			time.Sleep(time.Millisecond)
			return map[string]any{"resources": []any{map[string]any{"kind": "Kind" + p[len(p)-4:len(p)-3]}}}, nil
		}
		_, apis, err := k.readCapabilities(context.Background())
		return apis, err, peak.Load()
	}
	apis, err, peak := collect(nil)
	if err != nil || len(apis) != 82 || peak > discoveryConcurrency || peak < 2 {
		t.Fatal("discovery not bounded and complete", len(apis), err, peak)
	}
	again, _, _ := collect(nil)
	if !reflect.DeepEqual(apis, again) || !slicesSorted(apis) {
		t.Fatal("discovery results are not stable and sorted")
	}
	_, err, _ = collect(map[string]bool{"/apis/g03.example.com/v1": true, "/apis/g30.example.com/v1": true})
	var failure *domain.Failure
	if !errors.As(err, &failure) || failure.Issue.Reason != "API resources are missing" {
		t.Fatal("a later group's failure replaced the first failure in sorted order", err)
	}
}

func slicesSorted(values []any) bool {
	for i := 1; i < len(values); i++ {
		if values[i-1].(string) > values[i].(string) {
			return false
		}
	}
	return true
}
