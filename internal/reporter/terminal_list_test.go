package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
)

func TestTerminalListChangeShowsItems(t *testing.T) {
	rule := func(resource string) any {
		return map[string]any{"apiGroups": []any{""}, "resources": []any{resource}, "verbs": []any{"get"}}
	}
	long := strings.Repeat("x", 400)
	for _, tt := range []struct {
		name   string
		change domain.Change
		want   []string
		absent []string
	}{
		{"added", domain.Change{Path: "/rules", Operation: "replace", Before: []any{rule(long), rule("pods")}, After: []any{rule(long), rule("pods"), rule("services")}}, []string{"/rules: list changed (2 -> 3 items; 1 added, 0 removed)", `+ {"apiGroups":[""],"resources":["services"],"verbs":["get"]}`}, []string{long[:330]}},
		{"removed duplicate", domain.Change{Path: "/args", Operation: "replace", Before: []any{long, long, "b"}, After: []any{long, "b"}}, []string{"(3 -> 2 items; 0 added, 1 removed)", `- "xxx`}, nil},
		{"reordered", domain.Change{Path: "/args", Operation: "replace", Before: []any{long, "b"}, After: []any{"b", long}}, []string{"0 added, 0 removed; order changed"}, nil},
		{"short list stays whole", domain.Change{Path: "/ports", Operation: "replace", Before: []any{"a"}, After: []any{"b"}}, []string{`/ports: ["a"] -> ["b"]`}, []string{"list changed"}},
		{"redacted stays whole", domain.Change{Path: "/data", Operation: "replace", Before: []any{long}, After: []any{"y"}, Redacted: true}, []string{`/data: ["xxx`, `[redacted]`}, []string{"list changed"}},
		{"scalar stays whole", domain.Change{Path: "/replicas", Operation: "replace", Before: 1.0, After: 2.0}, []string{"/replicas: 1 -> 2"}, []string{"list changed"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var w terminalWriter
			terminalChange(&w, "   ", tt.change)
			out := w.b.String()
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q in\n%s", want, out)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(out, absent) {
					t.Fatalf("unexpected %q in\n%s", absent, out)
				}
			}
		})
	}
}
