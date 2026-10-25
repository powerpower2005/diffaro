package preview

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"diffaro/internal/profile"
)

// TestChangedPathReadsDoNotSerializeDecisions blocks the chart read of one
// directory and requires a decision on another directory to finish, and
// concurrent decisions on the blocked directory to share its single read.
func TestChangedPathReadsDoNotSerializeDecisions(t *testing.T) {
	inRead, release := make(chan struct{}), make(chan struct{})
	var slowReads atomic.Int32
	read := func(_ context.Context, file string) ([]byte, bool, error) {
		if file == "charts/slow/Chart.yaml" {
			if slowReads.Add(1) == 1 {
				close(inRead)
			}
			<-release
		}
		return []byte("apiVersion: v2\nname: chart\nversion: 0.1.0\n"), true, nil
	}
	present := map[string]bool{"charts/slow/Chart.yaml": true, "charts/fast/Chart.yaml": true}
	selector := newChangedPathSelector(ChangedPaths{Read: read, Listed: true, Present: present}, profile.Git{RepositoryURL: pathRepo}.Matches)
	no := false
	var slow sync.WaitGroup
	results := make([]string, 4)
	for i := range results {
		slow.Add(1)
		go func() {
			defer slow.Done()
			results[i], _ = selector.decide(context.Background(), pathApp(helmSource("charts/slow", nil)), &no)
		}()
	}
	<-inRead
	fast := make(chan string)
	go func() {
		got, _ := selector.decide(context.Background(), pathApp(helmSource("charts/fast", nil)), &no)
		fast <- got
	}()
	select {
	case got := <-fast:
		if got != selectionUnaffected {
			t.Fatal(got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a decision on another directory waited for a chart read in flight")
	}
	close(release)
	slow.Wait()
	for _, got := range results {
		if got != selectionUnaffected {
			t.Fatal(got)
		}
	}
	if slowReads.Load() != 1 {
		t.Fatalf("concurrent decisions read one chart %d times", slowReads.Load())
	}
}

// TestChangedPathWaiterRereadsAfterCanceledRead requires a caller that comes
// while a read is in flight, which is then canceled, to read again instead of
// sharing the canceled result.
func TestChangedPathWaiterRereadsAfterCanceledRead(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	read := func(ctx context.Context, _ string) ([]byte, bool, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return []byte("apiVersion: v2\nname: api\nversion: 0.1.0\n"), true, nil
	}
	present := map[string]bool{"charts/api/Chart.yaml": true}
	selector := newChangedPathSelector(ChangedPaths{Read: read, Listed: true, Present: present}, profile.Git{RepositoryURL: pathRepo}.Matches)
	no := false
	app := pathApp(helmSource("charts/api", nil))
	canceled, cancel := context.WithCancel(context.Background())
	first := make(chan string)
	go func() { got, _ := selector.decide(canceled, app, &no); first <- got }()
	<-started
	second := make(chan string)
	go func() { got, _ := selector.decide(context.Background(), app, &no); second <- got }()
	cancel()
	close(release)
	if got := <-first; got != selectionIneligible {
		t.Fatalf("a canceled read must fail closed: %s", got)
	}
	if got := <-second; got != selectionUnaffected || calls.Load() != 2 {
		t.Fatalf("the waiter shared a canceled read: %s after %d reads", got, calls.Load())
	}
}

// TestChangedPathWaiterHonorsItsOwnContext requires a caller that waits for
// another caller's chart read to stop when its own context ends, failing
// closed, while the read itself goes on and is kept for later callers.
func TestChangedPathWaiterHonorsItsOwnContext(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	read := func(context.Context, string) ([]byte, bool, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return []byte("apiVersion: v2\nname: api\nversion: 0.1.0\n"), true, nil
	}
	present := map[string]bool{"charts/api/Chart.yaml": true}
	selector := newChangedPathSelector(ChangedPaths{Read: read, Listed: true, Present: present}, profile.Git{RepositoryURL: pathRepo}.Matches)
	no := false
	app := pathApp(helmSource("charts/api", nil))
	first := make(chan string)
	go func() { got, _ := selector.decide(context.Background(), app, &no); first <- got }()
	<-started
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	waiter := make(chan string)
	go func() { got, _ := selector.decide(expired, app, &no); waiter <- got }()
	select {
	case got := <-waiter:
		if got != selectionIneligible {
			t.Fatalf("a waiter whose context ended must fail closed: %s", got)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("a waiter outlived its own context")
	}
	close(release)
	if got := <-first; got != selectionUnaffected {
		t.Fatal(got)
	}
	if got, _ := selector.decide(context.Background(), app, &no); got != selectionUnaffected || calls.Load() != 1 {
		t.Fatalf("the reading was not kept: %s after %d reads", got, calls.Load())
	}
}
