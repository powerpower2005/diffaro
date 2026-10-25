package reporter

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// objectOfWidth returns a single-line object whose one-line JSON is width
// characters long.
func objectOfWidth(t *testing.T, width int) map[string]any {
	t.Helper()
	empty, _ := json.Marshal(map[string]any{"args": []any{""}})
	object := map[string]any{"args": []any{strings.Repeat("a", width-len(empty))}}
	if data, _ := json.Marshal(object); len(data) != width {
		t.Fatalf("object is %d characters, want %d", len(data), width)
	}
	return object
}

// An added object without a multi-line string whose one-line JSON fits the
// PR value width keeps the one-line form; only a wider one would
// be cut and is shown as YAML lines.
func TestAddedObjectWithinPRWidthKeepsOneLineForm(t *testing.T) {
	for _, width := range []int{320, 399, 400} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			change := domain.Change{Path: "/spec/x", Operation: "add", After: objectOfWidth(t, width)}
			if _, ok := objectLines(change); ok {
				t.Fatal("object within the PR width was shown as YAML lines")
			}
			r := tailChangeReport("a", "b")
			r.Applications[0].Resources[0].Changes = []domain.Change{change}
			data, err := Encode(r, "markdown", security.Redactor{})
			if err != nil {
				t.Fatal(err)
			}
			if out := string(data); strings.Contains(out, "added (") || strings.Contains(out, "+args:") {
				t.Fatalf("markdown showed the object as lines:\n%s", out)
			}
			// PR Markdown shows every value as YAML lines.
			data, err = Encode(r, "pr-markdown", security.Redactor{})
			if err != nil {
				t.Fatal(err)
			}
			if out := string(data); !strings.Contains(out, "@@ /spec/x: added (2 lines) @@\n+args:\n") {
				t.Fatalf("pr-markdown did not show the added object as YAML lines:\n%s", out)
			}
		})
	}
	if _, ok := objectLines(domain.Change{Path: "/spec/x", Operation: "add", After: objectOfWidth(t, 401)}); !ok {
		t.Fatal("object past the PR width was not shown as YAML lines")
	}
}

// Object lines are bounded like text diffs: an object with more
// YAML lines than the bound shows the first lines and how many more are in
// JSON.
func TestAddedObjectLinesAreBounded(t *testing.T) {
	object := map[string]any{}
	for i := range 50 {
		object[fmt.Sprintf("key%02d", i)] = fmt.Sprintf("value%02d", i)
	}
	d, ok := objectLines(domain.Change{Path: "/spec/x", Operation: "add", After: object})
	if !ok {
		t.Fatal("object was not shown as lines")
	}
	if len(d.lines) != 40 || d.more != 10 || d.added != 50 {
		t.Fatalf("shown %d lines, %d more, %d added; want 40, 10, 50", len(d.lines), d.more, d.added)
	}
	if d.lines[0] != "+key00: value00" || d.lines[39] != "+key39: value39" {
		t.Fatalf("not the first 40 lines: %q ... %q", d.lines[0], d.lines[39])
	}
	var b strings.Builder
	writeTextDiff(&b, "", "`/spec/x`", d, 400)
	if out := b.String(); !strings.Contains(out, "10 more line(s) in JSON") || strings.Contains(out, "key40") {
		t.Fatalf("bounded lines or notice missing:\n%s", out)
	}
}
