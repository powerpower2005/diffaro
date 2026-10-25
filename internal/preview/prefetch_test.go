package preview

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"diffaro/internal/domain"
)

func TestWideAppOfAppsRendersChildrenConcurrentlyOnce(t *testing.T) {
	// A child render is held until three children of its revision are in
	// flight together. Background renders share one run-wide
	// render.maxConcurrency bound across base and head: 4 slots
	// can split 2+2 so neither revision ever reaches three. A bound above
	// 2×(3-1) leaves one revision a third slot, and once it fills its gate
	// its slots free up for the other. A walk without concurrent background
	// renders never fills the gate and gives up after the deadline.
	const overlap, maxConcurrency = 3, 5
	type gate struct {
		inFlight, peak int
		full           chan struct{}
		once           sync.Once
	}
	var mu sync.Mutex
	renders := map[string]int{}
	gates := map[string]*gate{graphBase: {full: make(chan struct{})}, graphHead: {full: make(chan struct{})}}
	var children []string
	for i := range 8 {
		children = append(children, appManifest(graphApp(fmt.Sprintf("child-%d", i))))
	}
	b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(ctx context.Context, a domain.Application, revision string) ([]string, error) {
		if a.ID.Name == "root" {
			return children, nil
		}
		mu.Lock()
		renders[revision+"/"+a.ID.Name]++
		g := gates[revision]
		g.inFlight++
		g.peak = max(g.peak, g.inFlight)
		if g.inFlight >= overlap {
			g.once.Do(func() { close(g.full) })
		}
		mu.Unlock()
		select {
		case <-g.full:
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
			g.once.Do(func() { close(g.full) })
		}
		mu.Lock()
		g.inFlight--
		mu.Unlock()
		return []string{configMap(revision)}, nil
	}}
	runner := graphRunner(b, "root")
	runner.Config.Render.MaxConcurrency, runner.Config.Render.Timeout = maxConcurrency, "30s"
	report := runner.RunRecursive(context.Background(), graphPair())
	if len(report.Applications) != 9 || !report.Graph.BaseComplete || !report.Graph.HeadComplete {
		t.Fatalf("wide graph incomplete: %d Applications", len(report.Applications))
	}
	for _, revision := range []string{graphBase, graphHead} {
		if peak := gates[revision].peak; peak < overlap {
			t.Fatalf("children of %s were not rendered concurrently (peak %d, want %d)", revision, peak, overlap)
		}
	}
	for key, count := range renders {
		if count != 1 {
			t.Fatalf("%s rendered %d times", key, count)
		}
	}
	if got := findApp(t, report, "child-7"); got.State != "changed" || got.Execution != "complete" {
		t.Fatalf("child result changed by concurrency: %s/%s", got.State, got.Execution)
	}
}
