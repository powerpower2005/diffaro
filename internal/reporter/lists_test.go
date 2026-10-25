package reporter

import (
	"fmt"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

// changeReport is one changed Deployment whose only field change is change.
func changeReport(change domain.Change) domain.Report {
	r := tailChangeReport("a", "b")
	r.Applications[0].Resources[0].Changes = []domain.Change{change}
	return r
}

// An unkeyed list whose JSON is at most 400 characters keeps its
// before/after pair in Markdown, while the terminal splits it into items past
// 320 characters.
func TestUnkeyedListSplitWidths(t *testing.T) {
	shared := strings.Repeat("a", 330)
	mid := domain.Change{Path: "/spec/template/spec/containers/0/args", Operation: "replace", Before: []any{shared, "old-item"}, After: []any{shared, "new-item"}}
	if n := len(`["` + shared + `","old-item"]`); n <= 320 || n > 400 {
		t.Fatalf("fixture precondition: list JSON is %d characters", n)
	}
	r := changeReport(mid)
	out := encodeText(t, r, "markdown")
	if strings.Contains(out, "list changed") {
		t.Fatalf("markdown split a list of at most 400 characters into items:\n%s", out)
	}
	if line := lineWith(out, "`/spec/template/spec/containers/0/args`"); !strings.Contains(line, "→") || !strings.Contains(line, "old-item") || !strings.Contains(line, "new-item") {
		t.Fatalf("markdown lost the before/after pair: %q", line)
	}
	// PR Markdown lists the items of any unkeyed list.
	pr := encodeText(t, r, "pr-markdown")
	if !strings.Contains(pr, "list changed (2 → 2 items; 1 added, 1 removed)") || !strings.Contains(pr, "\n-- old-item\n") || !strings.Contains(pr, "\n+- new-item\n") || strings.Contains(pr, shared) {
		t.Fatalf("pr-markdown did not show only the added and removed items:\n%s", pr)
	}
	terminal := encodeText(t, r, "terminal")
	if !strings.Contains(terminal, "list changed (2 -> 2 items; 1 added, 1 removed)") || !strings.Contains(terminal, `+ "new-item"`) || !strings.Contains(terminal, `- "old-item"`) {
		t.Fatalf("terminal did not split a list over 320 characters into items:\n%s", terminal)
	}

	long := strings.Repeat("a", 420)
	r = changeReport(domain.Change{Path: "/spec/template/spec/containers/0/args", Operation: "replace", Before: []any{long, "old-item"}, After: []any{long, "new-item"}})
	for _, format := range []string{"markdown", "pr-markdown"} {
		out := encodeText(t, r, format)
		if !strings.Contains(out, "list changed (2 → 2 items; 1 added, 1 removed)") {
			t.Fatalf("%s did not split a list over 400 characters into items:\n%s", format, out)
		}
	}
}

// Markdown shows at most 8 added items, then counts the rest.
func TestUnkeyedListShowsAtMostEightAddedItems(t *testing.T) {
	long := strings.Repeat("a", 420)
	after := []any{long}
	for i := 1; i <= 10; i++ {
		after = append(after, fmt.Sprintf("added-%02d", i))
	}
	r := changeReport(domain.Change{Path: "/spec/template/spec/containers/0/args", Operation: "replace", Before: []any{long}, After: after})
	for _, format := range []string{"markdown", "pr-markdown"} {
		out := encodeText(t, r, format)
		if !strings.Contains(out, "10 added, 0 removed") || !strings.Contains(out, "added-08") {
			t.Fatalf("%s lacks the added items:\n%s", format, out)
		}
		if !strings.Contains(out, "2 more added item(s) in JSON") {
			t.Fatalf("%s does not count the added items past 8:\n%s", format, out)
		}
		if strings.Contains(out, "added-09") || strings.Contains(out, "added-10") {
			t.Fatalf("%s lists more than 8 added items:\n%s", format, out)
		}
	}
}

// An input key whose value is a long unkeyed list is shown as its
// added and removed items, as resource fields are.
func TestInputKeyListShowsItems(t *testing.T) {
	long := strings.Repeat("a", 420)
	r := tailChangeReport("a", "b")
	r.Applications[0].InputChanges = []domain.InputChange{{Kind: "valueFile", Repository: "https://github.com/example/charts.git", Path: "charts/api/values.yaml", State: "changed", Comparison: "complete",
		Changes: []domain.Change{{Path: "/extraArgs", Operation: "replace", Before: []any{long, "--old-flag"}, After: []any{long, "--new-flag", "--extra-flag"}}}}}
	for _, format := range []string{"markdown", "pr-markdown"} {
		out := encodeText(t, r, format)
		line := lineWith(out, "`/extraArgs`")
		if !strings.Contains(line, "list changed (2 → 3 items; 2 added, 1 removed)") {
			t.Fatalf("%s does not show the input list as items: %q\n%s", format, line, out)
		}
		if !strings.Contains(out, "--new-flag") || !strings.Contains(out, "--old-flag") {
			t.Fatalf("%s lacks the added or removed input item:\n%s", format, out)
		}
	}
}
