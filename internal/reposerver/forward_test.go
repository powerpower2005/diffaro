package reposerver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

func TestForwardAddress(t *testing.T) {
	for _, value := range []string{"Forwarding from 0.0.0.0:8081 -> 8081", "Forwarding from 127.0.0.1:0 -> 8081", "Forwarding from 127.0.0.1:65536 -> 8081", "error with sensitive text"} {
		if forwardAddress(value) != "" {
			t.Fatal("accepted invalid listener")
		}
	}
	if forwardAddress("Forwarding from 127.0.0.1:32123 -> 8081") != "127.0.0.1:32123" {
		t.Fatal("lost ephemeral port")
	}
}

func TestPortForwardHelper(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-2] != "--forward-helper" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "ready":
		fmt.Println("Forwarding from 127.0.0.1:32123 -> 8081")
	case "fail":
		os.Exit(1)
	case "exit-after-ready":
		fmt.Println("Forwarding from 127.0.0.1:32123 -> 8081")
		time.Sleep(200 * time.Millisecond)
		os.Exit(1)
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestPortForwardLifecycle(t *testing.T) {
	for _, mode := range []string{"ready", "fail", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			p := &portForward{config: profile.RepoServer{Kubernetes: &profile.KubernetesContext{}, PortForward: &profile.PortForward{Port: 8081}}}
			var command *exec.Cmd
			starts := 0
			p.command = func(ctx context.Context) *exec.Cmd {
				starts++
				command = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPortForwardHelper$", "--", "--forward-helper", mode)
				return command
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			defer p.close()
			address, err := p.address(ctx)
			if mode == "ready" {
				if err != nil || address != "127.0.0.1:32123" {
					t.Fatalf("listener startup: %v", err)
				}
				if second, err := p.address(ctx); err != nil || second != address || starts != 1 {
					t.Fatal("forwarder was not reused")
				}
			} else if err == nil {
				t.Fatal("failed forward reported success")
			}
			p.close()
			if command.ProcessState == nil {
				t.Fatal("child process not reaped")
			}
			if _, err := p.address(context.Background()); err == nil {
				t.Fatal("closed forward restarted")
			}
		})
	}
}

// forwardFixture returns a forward whose nth start runs helper modes[n-1]
// (the last mode repeats) and signals started once per start.
func forwardFixture(modes ...string) (*portForward, *atomic.Int32, chan struct{}) {
	p := &portForward{config: profile.RepoServer{Kubernetes: &profile.KubernetesContext{}, PortForward: &profile.PortForward{Port: 8081}}}
	starts := &atomic.Int32{}
	started := make(chan struct{}, 8)
	p.command = func(ctx context.Context) *exec.Cmd {
		n := int(starts.Add(1))
		started <- struct{}{}
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPortForwardHelper$", "--", "--forward-helper", modes[min(n, len(modes))-1])
	}
	return p, starts, started
}

func waitStarted(t *testing.T, started chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("forward was not started")
	}
}

// A startup abandoned because its caller was cancelled (stage timeout or a
// cancelled sibling side) does not fail later callers: the next caller with a
// live context starts a new forward.
func TestPortForwardRestartsAfterCancelledStartup(t *testing.T) {
	p, starts, started := forwardFixture("cancel", "ready")
	defer p.close()
	first, cancelFirst := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := p.address(first); result <- err }()
	waitStarted(t, started)
	cancelFirst()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled starter returned %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if address, err := p.address(ctx); err != nil || address != "127.0.0.1:32123" || starts.Load() != 2 {
		t.Fatalf("live caller after cancelled startup: address %q, err %v, starts %d", address, err, starts.Load())
	}
	p.close()
	if _, err := p.address(ctx); err == nil || starts.Load() != 2 {
		t.Fatal("closed forward restarted")
	}
}

// A forward that exits after it was ready is reported as a failure
// on every later call and is not silently restarted.
func TestPortForwardExitAfterReadyFails(t *testing.T) {
	p, starts, _ := forwardFixture("exit-after-ready", "ready")
	defer p.close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := p.address(ctx); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	done := p.done
	p.mu.Unlock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("helper did not exit")
	}
	for range 2 {
		_, err := p.address(ctx)
		var failure *domain.Failure
		if !errors.As(err, &failure) || failure.Issue.Code != domain.CodeRepoServerFailed || failure.Issue.Stage != "connection" || starts.Load() != 1 {
			t.Fatalf("exited forward: err %v, starts %d", err, starts.Load())
		}
	}
}

// A caller waiting for another caller's startup returns when its own context
// ends, and closing during startup stops and reaps the starting process.
func TestPortForwardWaiterHonorsOwnContext(t *testing.T) {
	p, starts, started := forwardFixture("cancel")
	defer p.close()
	result := make(chan error, 1)
	go func() { _, err := p.address(context.Background()); result <- err }()
	waitStarted(t, started)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	begin := time.Now()
	if _, err := p.address(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(begin) > 5*time.Second {
		t.Fatalf("waiter returned %v after %v", err, time.Since(begin))
	}
	p.close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("closed startup reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("close did not stop the starting forward")
	}
	if _, err := p.address(context.Background()); err == nil || starts.Load() != 1 {
		t.Fatal("closed forward restarted")
	}
}
