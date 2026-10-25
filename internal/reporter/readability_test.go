package reporter

import (
	"fmt"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// tailChangeReport has one changed resource whose values share a long prefix
// and differ only at the end, the shape of an env entry appended to a list.
func tailChangeReport(before, after string) domain.Report {
	r := domain.NewReport()
	r.Status, r.ChangesFound = "changes_found", true
	app := domain.ApplicationResult{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}, State: "changed", Execution: "complete"}
	app.Resources = []domain.ResourceDiff{{
		ID:         domain.ResourceID{Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "api"},
		State:      "changed",
		Comparison: "complete",
		Changes:    []domain.Change{{Path: "/spec/template/spec/containers/0/args", Operation: "replace", Before: before, After: after}},
	}}
	r.Applications = []domain.ApplicationResult{app}
	return r
}

func TestEscapedCodeSpansRenderAsHTMLCode(t *testing.T) {
	report := tailChangeReport(`say "hi"`, `say "bye" | *now*`)
	for _, format := range []string{"markdown", "pr-markdown"} {
		data, err := Encode(report, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		// GitHub prints entities inside backtick code spans literally.
		if strings.Contains(out, "`&") || strings.Contains(out, ";`") {
			t.Fatalf("%s keeps an entity inside a backtick code span:\n%s", format, out)
		}
		// PR Markdown shows values literally in diff blocks.
		if format == "pr-markdown" {
			if !strings.Contains(out, "\n-say \"hi\"\n+say \"bye\" | *now*\n") {
				t.Fatalf("%s lost the literal value in its diff block:\n%s", format, out)
			}
		} else if !strings.Contains(out, "<code>&#34;say &#92;&#34;bye&#92;&#34; &#124; &#42;now&#42;&#34;</code>") {
			t.Fatalf("%s lost the escaped value in a <code> element:\n%s", format, out)
		}
		if format == "markdown" && !strings.Contains(out, "`/spec/template/spec/containers/0/args`") {
			t.Fatalf("%s rewrote a code span without entities:\n%s", format, out)
		}
	}
}

func TestLongPairsShowTheirFirstDifference(t *testing.T) {
	prefix := strings.Repeat("a", 5000)
	report := tailChangeReport(prefix+"-old", prefix+"-new")
	for _, format := range []string{"markdown", "pr-markdown", "terminal"} {
		data, err := Encode(report, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		if !strings.Contains(out, "-old") || !strings.Contains(out, "-new") {
			t.Fatalf("%s cut away the only difference:\n%.2000s", format, out)
		}
		if strings.Contains(out, strings.Repeat("a", 4001)) {
			t.Fatalf("%s did not bound the shared prefix", format)
		}
	}
}

func TestPairBoundsKeepShortAndUnrelatedValues(t *testing.T) {
	if before, after := prPair(`"a"`, `"b"`, 10); before != esc(`"a"`) || after != esc(`"b"`) {
		t.Fatalf("short pair changed: %q %q", before, after)
	}
	// Values that differ from the first rune keep the start of both sides.
	before, after := prPair(strings.Repeat("x", 200), strings.Repeat("y", 200), 20)
	if !strings.HasPrefix(before, "xxx") || !strings.HasPrefix(after, "yyy") || !strings.HasSuffix(after, "…") {
		t.Fatalf("unrelated pair lost its start: %q %q", before, after)
	}
	// A difference past the bound starts both sides shortly before it.
	before, after = prPair(strings.Repeat("x", 200)+"1", strings.Repeat("x", 200)+"2", 20)
	if before != "…xxxxx1" || after != "…xxxxx2" {
		t.Fatalf("tail difference not shown: %q %q", before, after)
	}
	b, a := terminalPair(strings.Repeat("x", 400)+"1", strings.Repeat("x", 400)+"2")
	if !strings.HasPrefix(b, `"...`) || !strings.HasSuffix(b, `1"`) || !strings.HasSuffix(a, `2"`) {
		t.Fatalf("terminal tail difference not shown: %q %q", b, a)
	}
}

func TestItemPathShowsListItemNames(t *testing.T) {
	for _, tc := range []struct {
		change domain.Change
		want   string
	}{
		{domain.Change{Path: "/spec/containers/0/env/57", ItemNames: []string{"backend", "PUSH_MODE"}}, "/spec/containers[backend]/env[PUSH_MODE]"},
		{domain.Change{Path: "/spec/containers/1/image", ItemNames: []string{"sidecar"}}, "/spec/containers[sidecar]/image"},
		// A numeric map key makes the index count disagree; keep the pointer.
		{domain.Change{Path: "/ports/80/items/0", ItemNames: []string{"a"}}, "/ports/80/items/0"},
		{domain.Change{Path: "/spec/replicas"}, "/spec/replicas"},
	} {
		if got := itemPath(tc.change); got != tc.want {
			t.Fatalf("itemPath(%s) = %s, want %s", tc.change.Path, got, tc.want)
		}
	}
}

// A whole list item starts with the field that names it in its change path:
// a rule named B by record, whose name repeats, starts with its record.
func TestWholeItemStartsWithTheFieldOfItsPathName(t *testing.T) {
	change := domain.Change{Path: "/spec/groups/0/rules/1", Operation: "add", ItemNames: []string{"g", "B"}, After: map[string]any{"name": "dup", "record": "B", "expr": "sum(b)"}}
	lines := fieldDiffLines(change)
	if len(lines) < 2 || lines[0] != "@@ /spec/groups[g]/rules[B]: added (3 lines) @@" || lines[1] != "+record: B" {
		t.Fatalf("added rule does not start with the record that names it: %q", lines)
	}
}

// A value hidden on both sides is reported as changed, never as an equal
// pair of masks.
func TestHiddenValueChangesReadAsChanged(t *testing.T) {
	r := tailChangeReport("a", "b")
	r.Applications[0].Resources[0].Changes = []domain.Change{{Path: "/data/key", Operation: "replace", Before: security.Mask, After: security.Mask, Redacted: true}}
	for _, format := range []string{"markdown", "pr-markdown", "terminal"} {
		data, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		if format == "pr-markdown" {
			// PR diff blocks mark the masked pair as changed.
			if !strings.Contains(out, "@@ /data/key @@\n-'[REDACTED]'  # value changed (hidden)\n+'[REDACTED]'  # value changed (hidden)\n") || unnotedMask(out) {
				t.Fatalf("%s does not say the hidden value changed:\n%s", format, out)
			}
			continue
		}
		if !strings.Contains(out, "/data/key") || !strings.Contains(out, "value changed (hidden)") || strings.Contains(out, security.Mask) {
			t.Fatalf("%s does not say the hidden value changed:\n%s", format, out)
		}
	}
}

// A list without item keys is shown as its added and removed items when the
// whole list is too long to read as one pair.
func TestUnkeyedListChangesShowAddedAndRemovedItems(t *testing.T) {
	rule := func(resource string) any {
		return map[string]any{"apiGroups": []any{""}, "resources": []any{resource}, "verbs": []any{"get", "list", "watch"}}
	}
	var before []any
	for i := 0; i < 12; i++ {
		before = append(before, rule(fmt.Sprintf("existing-%02d", i)))
	}
	after := append(append([]any{}, before[1:]...), rule("services"), rule("gateways"))
	r := tailChangeReport("a", "b")
	r.Applications[0].Resources[0].Changes = []domain.Change{{Path: "/rules", Operation: "replace", Before: before, After: after}}
	for _, format := range []string{"markdown", "pr-markdown", "terminal"} {
		data, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		for _, want := range []string{"list changed", "2 added, 1 removed", "services", "gateways", "existing-00"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s lacks %q:\n%s", format, want, out)
			}
		}
		if strings.Contains(out, "existing-05") {
			t.Fatalf("%s repeats unchanged items instead of listing the difference", format)
		}
	}
}

// Display values keep &, < and > as written: a Traefik match rule must read
// `&&`, not the \u0026 form json.Marshal produces. Markdown still escapes HTML,
// so a value cannot inject markup.
func TestDisplayValuesKeepRuleOperators(t *testing.T) {
	rule := func(path string) map[string]any {
		return map[string]any{"kind": "Rule", "match": "Host(`app.example.test`) && Path(`" + path + "`)", "note": "<b>x</b> > 1", "pad": strings.Repeat("r", 420)}
	}
	report := tailChangeReport("", "")
	resource := &report.Applications[0].Resources[0]
	resource.ID = domain.ResourceID{Group: "traefik.io", Kind: "IngressRoute", Namespace: "demo", Name: "api"}
	resource.Changes = []domain.Change{
		{Path: "/spec/rule", Operation: "replace", Before: map[string]any{"match": "Host(`a`) && Path(`/old`)"}, After: map[string]any{"match": "Host(`a`) && Path(`/new`)"}},
		{Path: "/spec/routes", Operation: "replace", Before: []any{rule("/a")}, After: []any{rule("/a"), rule("/webhooks")}},
		{Path: "/spec/long", Operation: "replace", Before: map[string]any{"match": "Path(`/old-long`)", "pad": strings.Repeat("p", 340)}, After: map[string]any{"match": "Path(`/new-long`) && Method(`GET`)", "pad": strings.Repeat("p", 340)}},
	}
	report.Applications[0].Resources = append(report.Applications[0].Resources, domain.ResourceDiff{
		ID: domain.ResourceID{Kind: "Service", Namespace: "demo", Name: "hook"}, State: "added", Comparison: "complete",
		Changes: []domain.Change{{Path: "/", Operation: "add", After: map[string]any{"kind": "Service", "spec": map[string]any{"selector": map[string]any{"rule": "Path(`/svc`) && Host(`b`)"}}}}},
	})
	report.Applications[0].SpecChanges = []domain.Change{{Path: "/metadata/annotations", Operation: "replace", Before: map[string]any{"rule": "Path(`/spec-old`)"}, After: map[string]any{"rule": "Path(`/spec-new`) && Host(`c`)"}}}
	report.Applications[0].InputChanges = []domain.InputChange{{Kind: "valueFile", Repository: "example/charts", Path: "values.yaml", State: "changed", Comparison: "complete", Changes: []domain.Change{
		{Path: "/ingress/extraRule", Operation: "add", After: map[string]any{"match": "Method(`POST`) && Path(`/hook`)"}},
	}}}
	for _, format := range []string{"markdown", "pr-markdown", "terminal"} {
		data, err := Encode(report, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		for _, escaped := range []string{"u0026", "u003c", "u003e"} {
			if strings.Contains(out, escaped) {
				t.Fatalf("%s shows a JSON HTML escape %q instead of the character:\n%s", format, escaped, out)
			}
		}
		operator, wants := "&&", []string{"Path(`/new`)", "/webhooks", "/new-long"}
		switch format {
		case "markdown":
			operator, wants = "&amp;&amp;", []string{"Path(&#96;/new&#96;)", "/webhooks", "/new-long", "Path(&#96;/hook&#96;)", "Path(&#96;/svc&#96;)", "Path(&#96;/spec-new&#96;)"}
		case "pr-markdown":
			// Diff blocks show values literally; text outside them is escaped.
			wants = []string{"Path(`/new`)", "/webhooks", "/new-long", "Path(`/svc`)", "Path(`/spec-new`)"}
			if outside := outsideFences(out); !strings.Contains(outside, "Path(&#96;/hook&#96;)") || !strings.Contains(outside, "&amp;&amp;") {
				t.Fatalf("%s lost the escaped input value:\n%s", format, out)
			}
		}
		if format != "terminal" && strings.Contains(outsideFences(out), "<b>") {
			t.Fatalf("%s let a value inject HTML:\n%s", format, out)
		}
		for _, want := range wants {
			if !strings.Contains(out, want) {
				t.Fatalf("%s lost %q:\n%s", format, want, out)
			}
		}
		if strings.Count(out, operator) < len(wants) {
			t.Fatalf("%s does not show the rule operator %q for every changed value:\n%s", format, operator, out)
		}
	}
}

// outsideFences drops the content of fenced diff blocks, which Markdown shows
// literally, so a check for markup covers only text that Markdown renders.
func outsideFences(markdown string) string {
	var b strings.Builder
	fence := ""
	for _, line := range strings.SplitAfter(markdown, "\n") {
		var literal bool
		if fence, literal = textFence(fence, line); !literal {
			b.WriteString(line)
		}
	}
	return b.String()
}

// unnotedMask reports a masked line in a diff block that is not noted as a
// hidden change, which would read as an unchanged value.
func unnotedMask(markdown string) bool {
	for _, line := range strings.Split(markdown, "\n") {
		if (strings.HasPrefix(line, "-") || strings.HasPrefix(line, "+")) && strings.Contains(line, security.Mask) && !strings.HasSuffix(line, "  # "+hiddenText) {
			return true
		}
	}
	return false
}
