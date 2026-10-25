package reposerver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

// A context read the Kubernetes API denies or does not find fails every
// later Application of the run without running kubectl again; any other read
// failure is retried by the next Application. The kubectl diagnostic only
// classifies the failure and never appears in it.
func TestKubernetesContextKeepsOnlyDeterministicReadFailures(t *testing.T) {
	for _, tc := range []struct {
		diagnostic string
		reads      int32
	}{
		{`Error from server (Forbidden): secrets is forbidden: User "fictional-reader" cannot list resource "secrets"`, 2},
		{`Error from server (NotFound): configmaps "argocd-cm" not found`, 1},
		{`Unable to connect to the server: dial tcp 192.0.2.1:443: i/o timeout`, 3 * 2},
	} {
		t.Run(strings.SplitN(tc.diagnostic, ":", 2)[0], func(t *testing.T) {
			k := newKubernetesContext(profile.KubernetesContext{Namespace: "argocd"})
			var reads atomic.Int32
			notFound := strings.Contains(tc.diagnostic, "NotFound")
			k.read = func(_ context.Context, args ...string) (map[string]any, error) {
				reads.Add(1)
				if notFound || strings.Contains(strings.Join(args, " "), "secrets") {
					return nil, contextReadFailure([]byte(tc.diagnostic))
				}
				return map[string]any{"data": map[string]any{}}, nil
			}
			for i := range 3 {
				err := k.load(context.Background())
				var failure *domain.Failure
				if !errors.As(err, &failure) || failure.Issue.Code != domain.CodeContextReadFailed || failure.Issue.Reason != "Kubernetes render context read failed" {
					t.Fatalf("application %d: %v", i, err)
				}
				if strings.Contains(fmt.Sprintf("%v %+v", err, failure.Issue), "fictional-reader") || strings.Contains(err.Error(), "192.0.2.1") {
					t.Fatal("kubectl diagnostic leaked into the failure")
				}
			}
			if reads.Load() != tc.reads {
				t.Fatalf("kubectl reads=%d, want %d", reads.Load(), tc.reads)
			}
		})
	}
}

// kubectl's Forbidden answer on stderr is what classifies a real read as
// deterministic: five Applications run kubectl twice (ConfigMap, then the
// denied Secret list), not twice each.
func TestDeniedKubectlReadRunsOncePerRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake kubectl is a POSIX shell script")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho x >> " + calls + "\ncase \"$*\" in\n*configmap*) echo '{\"data\":{}}' ;;\n*) echo 'Error from server (Forbidden): secrets is forbidden' >&2; exit 1 ;;\nesac\n"
	kubectl := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(kubectl, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	k := newKubernetesContext(profile.KubernetesContext{Kubectl: kubectl, Kubeconfig: "local.yaml", Context: "local", Namespace: "argocd"})
	for range 5 {
		if err := k.load(context.Background()); err == nil || !strings.Contains(err.Error(), "Kubernetes render context read failed") {
			t.Fatalf("denied read did not fail: %v", err)
		}
	}
	if data, _ := os.ReadFile(calls); strings.Count(string(data), "x") != 2 {
		t.Fatalf("kubectl ran %d times", strings.Count(string(data), "x"))
	}
}

// A caller waiting for another caller's context collection stops at its own
// deadline instead of waiting for the collection to end.
func TestKubernetesContextWaiterHonorsItsContext(t *testing.T) {
	k := newKubernetesContext(profile.KubernetesContext{Namespace: "argocd"})
	started, release := make(chan struct{}), make(chan struct{})
	k.read = func(context.Context, ...string) (map[string]any, error) {
		close(started)
		<-release
		return nil, errors.New("collection ended")
	}
	go func() { _ = k.load(context.Background()) }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	waited := make(chan error, 1)
	go func() { waited <- k.load(ctx) }()
	select {
	case err := <-waited:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiter returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter ignored its deadline")
	}
	close(release)
}
