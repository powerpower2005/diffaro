package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
)

// Full Markdown states graph discovery completeness once, in the graph
// section with its edges.
func TestMarkdownGraphCompletenessStatedOnce(t *testing.T) {
	r := tailChangeReport("a", "b")
	parent := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "platform"}
	r.Graph = &domain.GraphSummary{BaseComplete: true, HeadComplete: false, HeadEdges: []domain.GraphEdge{{Parent: parent, Child: r.Applications[0].ID, Method: "rendered-application"}}}
	out := encodeText(t, r, "markdown")
	if n := strings.Count(out, "Graph discovery: base complete"); n != 1 {
		t.Fatalf("graph completeness stated %d times:\n%s", n, out)
	}
	line := lineWith(out, "Graph discovery: base complete")
	if !strings.Contains(line, "base complete `true`") || !strings.Contains(line, "head complete `false`") {
		t.Fatalf("graph completeness line lost a side: %q", line)
	}
	if !strings.Contains(out, "Analysis coverage is incomplete") {
		t.Fatalf("incomplete head graph no longer marks coverage incomplete:\n%s", out)
	}
}

// The side an added or removed value lacks reads as absent, as in the
// terminal, so it stays distinct from an explicit null; resource fields and
// input keys alike.
func TestMarkdownAbsentSideIsNotNull(t *testing.T) {
	absent := esc(absentText)
	for _, tt := range []struct {
		name          string
		change        domain.Change
		before, after string
	}{
		{"added value", domain.Change{Path: "/spec/foo", Operation: "add", After: "v"}, absent, "<code>&#34;v&#34;</code>"},
		{"removed value", domain.Change{Path: "/spec/foo", Operation: "remove", Before: "v"}, "<code>&#34;v&#34;</code>", absent},
		{"added explicit null", domain.Change{Path: "/spec/foo", Operation: "add", After: nil}, absent, "`null`"},
		{"removed explicit null", domain.Change{Path: "/spec/foo", Operation: "remove", Before: nil}, "`null`", absent},
		{"replaced null", domain.Change{Path: "/spec/foo", Operation: "replace", Before: nil, After: 1.0}, "`null`", "`1`"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := changeReport(tt.change)
			r.Applications[0].InputChanges = []domain.InputChange{{Kind: "valueFile", Repository: "https://github.com/example/charts.git", Path: "charts/api/values.yaml", State: "changed", Comparison: "complete", Changes: []domain.Change{tt.change}}}
			want := tt.before + " → " + tt.after
			out := encodeText(t, r, "markdown")
			if line := lineWith(out, "`/spec/foo` ("+string(tt.change.Operation)+")"); !strings.HasSuffix(line, ": "+want) {
				t.Fatalf("resource change reads %q, want %q", line, want)
			}
			for _, format := range []string{"markdown", "pr-markdown"} {
				out := encodeText(t, r, format)
				if line := lineWith(out, "  - `/spec/foo`: "); !strings.HasSuffix(line, ": "+want) {
					t.Fatalf("%s input key reads %q, want %q", format, line, want)
				}
			}
		})
	}
}

// A whole added or removed resource shows its familiar settings and points
// to JSON for the manifest instead of printing the manifest as one JSON pair.
func TestMarkdownWholeResourceShowsHighlightsNotManifestJSON(t *testing.T) {
	manifest := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "api", "annotations": map[string]any{"example.invalid/note": "only-in-json"}},
		"spec": map[string]any{"replicas": 2.0, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "api", "image": "example.invalid/api:1"}}}}}}
	for _, tt := range []struct {
		change domain.Change
		side   string
	}{
		{domain.Change{Path: "/", Operation: "add", After: manifest}, "Head"},
		{domain.Change{Path: "/", Operation: "remove", Before: manifest}, "Base"},
	} {
		out := encodeText(t, changeReport(tt.change), "markdown")
		for _, want := range []string{
			"- " + tt.side + " target manifest (" + string(tt.change.Operation) + "; full manifest in JSON)",
			"  - " + tt.side + " target `/spec/replicas`: `2`",
			"  - " + tt.side + " container <code>&#34;api&#34;</code>: image <code>&#34;example.invalid/api:1&#34;</code>",
		} {
			if !strings.Contains(out, want+"\n") {
				t.Fatalf("%s resource lacks %q:\n%s", tt.change.Operation, want, out)
			}
		}
		if strings.Contains(out, "only-in-json") || strings.Contains(out, "`/` (") {
			t.Fatalf("%s resource printed its whole manifest:\n%s", tt.change.Operation, out)
		}
	}
}

// Every format lists the removed items of a whole-list change before the
// added ones, for resource fields and input keys alike.
func TestListItemsRemovedBeforeAddedInEveryFormat(t *testing.T) {
	long := strings.Repeat("a", 420)
	change := domain.Change{Path: "/spec/args", Operation: "replace", Before: []any{long, "removed-item"}, After: []any{long, "added-item"}}
	r := changeReport(change)
	r.Applications[0].InputChanges = []domain.InputChange{{Kind: "valueFile", Repository: "https://github.com/example/charts.git", Path: "charts/api/values.yaml", State: "changed", Comparison: "complete",
		Changes: []domain.Change{{Path: "/extraArgs", Operation: "replace", Before: []any{long, "removed-key-item"}, After: []any{long, "added-key-item"}}}}}
	for _, tt := range []struct{ format, removed, added string }{
		{"markdown", "removed-item", "added-item"},
		{"pr-markdown", "removed-item", "added-item"},
		{"terminal", "removed-item", "added-item"},
		{"markdown", "removed-key-item", "added-key-item"},
		{"pr-markdown", "removed-key-item", "added-key-item"},
	} {
		out := encodeText(t, r, tt.format)
		removed, added := strings.Index(out, tt.removed), strings.Index(out, tt.added)
		if removed < 0 || added < 0 || removed > added {
			t.Fatalf("%s lists %q at %d and %q at %d, want removed first:\n%s", tt.format, tt.removed, removed, tt.added, added, out)
		}
	}
}

// A resource change shown as lines names its own operation: an added or
// removed object is not labeled a replacement.
func TestMarkdownLineChangeLabelNamesOperation(t *testing.T) {
	object := map[string]any{"config": strings.Repeat("x", 420)}
	for _, tt := range []struct {
		change      domain.Change
		label, view string
	}{
		{domain.Change{Path: "/spec/foo", Operation: "add", After: object}, "`/spec/foo` (add)", "added (1 lines)"},
		{domain.Change{Path: "/spec/foo", Operation: "remove", Before: object}, "`/spec/foo` (remove)", "removed (1 lines)"},
		{domain.Change{Path: "/spec/foo", Operation: "replace", Before: "a\nb\n", After: "a\nc\n"}, "`/spec/foo` (replace)", "text changed (2 → 2 lines; 1 added, 1 removed)"},
		{domain.Change{Path: "/spec/foo", Operation: "replace", Before: []any{strings.Repeat("x", 420), "old"}, After: []any{strings.Repeat("x", 420), "new"}}, "`/spec/foo` (replace)", "list changed"},
	} {
		out := encodeText(t, changeReport(tt.change), "markdown")
		line := lineWith(out, "`/spec/foo`")
		if !strings.HasPrefix(line, "- "+tt.label+": "+tt.view) {
			t.Fatalf("%s change labeled %q, want %q:\n%s", tt.change.Operation, line, tt.label+": "+tt.view, out)
		}
	}
}

// With no Application compared, full Markdown prints no empty overview table
// and points to failures only when the run has some.
func TestMarkdownOmitsEmptyOverviewTable(t *testing.T) {
	r := domain.NewReport()
	r.Status = "success"
	out := encodeText(t, r, "markdown")
	if strings.Contains(out, "| Argo CD |") || strings.Contains(out, "See failures above") || !strings.Contains(out, "No Applications were compared.") {
		t.Fatalf("empty run shows an empty table or names absent failures:\n%s", out)
	}
	r.Status = "failure"
	r.Issues = []domain.Issue{{Code: "permission_denied", Stage: "discovery", Reason: "applications list denied", Action: "grant applications get"}}
	if out := encodeText(t, r, "markdown"); !strings.Contains(out, "No Applications were compared. See failures above.") {
		t.Fatalf("a failed run without Applications must point to its failures:\n%s", out)
	}
	if out := encodeText(t, layoutReport(), "markdown"); !strings.Contains(out, "| Argo CD | Destination cluster |") {
		t.Fatalf("overview table is missing when Applications need review:\n%s", out)
	}
}

// A resource whose comparison is incomplete says why in every human format:
// a Secret Argo CD masked names the masking and the backends that compare
// it; a resource missing on a partial side names the partial render.
func TestIncompleteComparisonsSayWhy(t *testing.T) {
	id := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}
	r := domain.NewReport()
	r.Applications = []domain.ApplicationResult{{ID: id, Root: id, GraphPath: []domain.ApplicationID{id}, State: domain.StateUnknown, Execution: domain.ExecutionComplete, Resources: []domain.ResourceDiff{
		{ID: domain.ResourceID{Kind: "Secret", Namespace: "demo", Name: "credentials"}, State: domain.StateUnknown, Changes: []domain.Change{}, Comparison: domain.ComparisonIncomplete, IncompleteReason: domain.IncompleteMaskedSecret},
		{ID: domain.ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: "settings"}, State: domain.StateUnknown, Changes: []domain.Change{}, Comparison: domain.ComparisonIncomplete, IncompleteReason: domain.IncompletePartialTarget},
	}}}
	for _, format := range []string{"pr-markdown", "markdown", "terminal"} {
		out := encodeText(t, r, format)
		for _, want := range []string{"Argo CD's Application API masks Secret values", "Local Helm or repo-server compare them", "rendered only partially"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s lacks %q:\n%s", format, want, out)
			}
		}
	}
}

// Full Markdown lists changed and unknown edges with their base and head
// evidence and gives changed resources a section; unchanged edges are
// counted and unchanged resources named in one line, and JSON keeps them all.
func TestMarkdownSummarizesUnchangedEdgesAndResources(t *testing.T) {
	app := func(name string) domain.ApplicationID {
		return domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: name}
	}
	edge := func(child string) domain.GraphEdge {
		return domain.GraphEdge{Parent: app("root"), Child: app(child), Method: "rendered-application", Routing: "explicit-default", Repository: "https://git.example.com/charts", Revision: strings.Repeat("a", 40)}
	}
	r := domain.NewReport()
	r.Graph = &domain.GraphSummary{BaseComplete: true, HeadComplete: true,
		BaseEdges: []domain.GraphEdge{edge("kept-1"), edge("kept-2"), edge("gone")},
		HeadEdges: []domain.GraphEdge{edge("kept-1"), edge("kept-2"), edge("new")},
		Changes: []domain.GraphEdgeChange{
			{Parent: app("root"), Child: app("gone"), State: domain.StateRemoved},
			{Parent: app("root"), Child: app("kept-1"), State: domain.StateUnchanged},
			{Parent: app("root"), Child: app("kept-2"), State: domain.StateUnchanged},
			{Parent: app("root"), Child: app("new"), State: domain.StateAdded},
		}}
	resource := func(name string, state domain.State) domain.ResourceDiff {
		return domain.ResourceDiff{ID: domain.ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: name}, State: state, Changes: []domain.Change{}, Comparison: domain.ComparisonComplete}
	}
	changed := resource("changed", domain.StateChanged)
	changed.Changes = []domain.Change{{Path: "/data/x", Operation: domain.OperationReplace, Before: "1", After: "2"}}
	r.Applications = []domain.ApplicationResult{{ID: app("root"), Root: app("root"), GraphPath: []domain.ApplicationID{app("root")}, State: domain.StateChanged, Execution: domain.ExecutionComplete,
		Resources: []domain.ResourceDiff{changed, resource("same-a", domain.StateUnchanged), resource("same-b", domain.StateUnchanged)}}}
	out := encodeText(t, r, "markdown")
	for _, want := range []string{
		"- Edge **removed**: `east/argocd/root` → `east/argocd/gone`",
		"- Edge **added**: `east/argocd/root` → `east/argocd/new`",
		"- 2 unchanged edge(s); JSON lists them",
		"- base: `east/argocd/root` → `east/argocd/gone`",
		"- head: `east/argocd/root` → `east/argocd/new`",
		"ConfigMap/demo/changed — changed",
		"Unchanged resources (2): `ConfigMap/demo/same-a`, `ConfigMap/demo/same-b`.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("full Markdown lacks %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{"Edge **unchanged**", "`east/argocd/kept-1` ·", "same-a — unchanged"} {
		if strings.Contains(out, absent) {
			t.Fatalf("full Markdown still lists unchanged detail %q:\n%s", absent, out)
		}
	}
	if data := encodeText(t, r, "json"); !strings.Contains(data, "kept-2") || !strings.Contains(data, "same-b") {
		t.Fatal("JSON lost unchanged evidence")
	}
}
