package localhelm

import (
	"context"
	"testing"
)

// The owner of a key settles it once; a waiter reuses a kept render, and a
// render that is not kept or over the byte budget is not shared.
func TestRenderMemoSharesOnlyKeptRendersWithinItsBudget(t *testing.T) {
	var m renderMemo[string, string]
	entry, owner := m.claim("a")
	if !owner {
		t.Fatal("first caller must own the key")
	}
	waiter, owner := m.claim("a")
	if owner || waiter != entry {
		t.Fatal("a second caller must wait for the running render")
	}
	m.settle("a", entry, true, 4, 8, func() string { return "kept" })
	if value, kept, err := waiter.wait(context.Background()); err != nil || !kept || value != "kept" {
		t.Fatalf("kept render not shared: %q %t %v", value, kept, err)
	}
	over, _ := m.claim("b")
	m.settle("b", over, true, 5, 8, func() string { t.Fatal("render over the budget kept"); return "" })
	if _, kept, _ := over.wait(context.Background()); kept {
		t.Fatal("render over the budget shared")
	}
	failed, _ := m.claim("c")
	m.settle("c", failed, false, 1, 8, func() string { t.Fatal("failed render kept"); return "" })
	if _, owner := m.claim("c"); !owner {
		t.Fatal("a render that was not kept must be rendered again")
	}
	running, _ := m.claim("d")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := running.wait(ctx); err != context.Canceled {
		t.Fatalf("a cancelled waiter must stop waiting: %v", err)
	}
}
