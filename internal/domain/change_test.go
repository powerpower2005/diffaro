package domain

import (
	"slices"
	"testing"
)

// UnstablePaths names every RenderUnstable value by its JSON pointer below the
// prefix, escaping "~" and "/" in keys (RFC 6901), in sorted order.
func TestUnstablePathsNamesEverySentinel(t *testing.T) {
	v := map[string]any{
		"data": map[string]any{"password": RenderUnstable, "user": "admin", "a/b": RenderUnstable, "c~d": RenderUnstable},
		"list": []any{"kept", map[string]any{"token": RenderUnstable}, []any{RenderUnstable}},
		"same": "<differs between identical renders> ",
	}
	got := UnstablePaths("/spec", v)
	want := []string{"/spec/data/a~1b", "/spec/data/c~0d", "/spec/data/password", "/spec/list/1/token", "/spec/list/2/0"}
	if !slices.Equal(got, want) {
		t.Fatalf("UnstablePaths = %q, want %q", got, want)
	}
	if got := UnstablePaths("", RenderUnstable); !slices.Equal(got, []string{""}) {
		t.Fatalf("a sentinel at the root is named by the prefix: %q", got)
	}
	if got := UnstablePaths("", map[string]any{"a": []any{1, "x"}}); len(got) != 0 {
		t.Fatalf("no sentinel, got %q", got)
	}
}

// GroupIssues merges issues that differ only in side and revision and keeps
// issues that differ in anything else apart.
func TestGroupIssuesMergesSidesOnly(t *testing.T) {
	base := Issue{Code: CodeLocalRenderFailed, Stage: "render", Reason: "missing", Side: "base", Revision: "b"}
	head := base
	head.Side, head.Revision = "head", "h"
	other := head
	other.Reason = "different"
	got := GroupIssues([]Issue{base, head, base, other})
	if len(got) != 2 || got[0].Side != "base, head" || got[0].Revision != "b, h" || got[1].Reason != "different" || got[1].Side != "head" {
		t.Fatalf("GroupIssues = %+v", got)
	}
}
