package reporter

import (
	"fmt"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

func differingTexts(n int) (string, string) {
	var before, after strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&before, "base-%04d\n", i)
		fmt.Fprintf(&after, "head-%04d\n", i)
	}
	return before.String(), after.String()
}

// A differing middle of more than 1,000,000 line pairs is not
// diffed line by line; the change keeps its before/after pair.
func TestLargeDifferingMiddleKeepsThePair(t *testing.T) {
	before, after := differingTexts(900)
	if _, ok := lineView(domain.Change{Path: "/data/script", Operation: "replace", Before: before, After: after}); !ok {
		t.Fatal("fixture precondition: 900 x 900 differing lines must still be diffed")
	}
	before, after = differingTexts(1100)
	change := domain.Change{Path: "/spec/template/spec/containers/0/args", Operation: "replace", Before: before, After: after}
	if _, ok := lineView(change); ok {
		t.Fatal("1100 x 1100 differing lines were diffed line by line")
	}
	r := changeReport(change)
	for _, format := range []string{"markdown", "pr-markdown"} {
		out := encodeText(t, r, format)
		if strings.Contains(out, "text changed") {
			t.Fatalf("%s shows a line diff past the cell bound", format)
		}
		if format == "pr-markdown" {
			if !strings.Contains(out, "@@ /spec/template/spec/containers/0/args @@\n-|\n-  base-0000\n") || !strings.Contains(out, "\n+|\n+  head-0000\n") {
				t.Fatalf("pr-markdown lost the base or head value:\n%.3000s", out)
			}
			continue
		}
		if line := lineWith(out, "`/spec/template/spec/containers/0/args`"); !strings.Contains(line, "→") {
			t.Fatalf("%s lost the before/after pair: %q", format, line)
		}
	}
}

// A diff line is cut at 240 characters in PR Markdown and at 400
// in full Markdown.
func TestLineDiffCutWidths(t *testing.T) {
	r := tailChangeReport("keep\n"+strings.Repeat("y", 500)+"\nend\n", "keep\n"+strings.Repeat("z", 500)+"\nend\n")
	for _, tc := range []struct {
		format string
		width  int
	}{{"pr-markdown", 240}, {"markdown", 400}} {
		out := encodeText(t, r, tc.format)
		if !strings.Contains(out, "text changed (3 → 3 lines; 1 added, 1 removed)") {
			t.Fatalf("%s does not show the line diff:\n%s", tc.format, out)
		}
		// The width counts the leading +/- mark.
		cut := "+" + strings.Repeat("z", tc.width-1) + "…"
		if !strings.Contains(out, cut) || strings.Contains(out, strings.Repeat("z", tc.width)) {
			t.Fatalf("%s does not cut a 500-character line at %d characters", tc.format, tc.width)
		}
	}
}

// A multi-line string input key reads as a line diff.
func TestInputKeyMultiLineStringIsLineDiff(t *testing.T) {
	r := tailChangeReport("a", "b")
	r.Applications[0].InputChanges = []domain.InputChange{{Kind: "valueFile", Repository: "https://github.com/example/charts.git", Path: "charts/api/values.yaml", State: "changed", Comparison: "complete",
		Changes: []domain.Change{{Path: "/config", Operation: "replace", Before: "listen: 80\nworkers: 2\nlog: info\n", After: "listen: 80\nworkers: 4\nlog: info\n"}}}}
	for _, format := range []string{"markdown", "pr-markdown"} {
		out := encodeText(t, r, format)
		line := lineWith(out, "`/config`")
		if !strings.Contains(line, "text changed (3 → 3 lines; 1 added, 1 removed)") {
			t.Fatalf("%s does not show the input key as a line diff: %q\n%s", format, line, out)
		}
		if !strings.Contains(out, "+workers: 4") || !strings.Contains(out, "-workers: 2") {
			t.Fatalf("%s lacks the changed input lines:\n%s", format, out)
		}
	}
}
