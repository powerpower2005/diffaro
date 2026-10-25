package reporter

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

func numberedLines(prefix string, n int) []string {
	var lines []string
	for i := 1; i <= n; i++ {
		lines = append(lines, fmt.Sprintf("%s%02d", prefix, i))
	}
	return lines
}

// TestTextLineDiffFindsChangedLines verifies that a replaced multi-line
// string is compared line by line, with two lines of context.
func TestTextLineDiffFindsChangedLines(t *testing.T) {
	base := numberedLines("line", 20)
	head := append([]string{}, base...)
	head[9] = "line10 edited" // line 10 replaced
	head = append(head, "line21")
	d, ok := textLineDiff(domain.Change{Operation: "replace", Before: strings.Join(base, "\n") + "\n", After: strings.Join(head, "\n") + "\n"})
	if !ok {
		t.Fatal("multi-line string was not compared by line")
	}
	if d.beforeLines != 20 || d.afterLines != 21 || d.added != 2 || d.removed != 1 || d.more != 0 {
		t.Fatalf("counts: %+v", d)
	}
	got := strings.Join(d.lines, "\n")
	// The replaced line is shown as removed then added, with lines 8-9 and
	// 11-12 as context; the appended line with lines 19-20.
	want := strings.Join([]string{
		"@@ -8 +8 @@", " line08", " line09", "-line10", "+line10 edited", " line11", " line12",
		"@@ -19 +19 @@", " line19", " line20", "+line21",
	}, "\n")
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestTextLineDiffAppliesOnlyToReadableMultiLineStrings(t *testing.T) {
	multi := "a\nb\n"
	for _, tc := range []struct {
		name   string
		change domain.Change
	}{
		{"redacted", domain.Change{Operation: "replace", Before: multi, After: "a\nc\n", Redacted: true}},
		{"single line", domain.Change{Operation: "replace", Before: "a", After: "b"}},
		{"not a string", domain.Change{Operation: "replace", Before: multi, After: 1.0}},
		{"added value", domain.Change{Operation: "add", After: multi}},
		{"only the final newline differs", domain.Change{Operation: "replace", Before: "a\nb\n", After: "a\nb"}},
		{"over the line bound", domain.Change{Operation: "replace", Before: strings.Repeat("x\n", textDiffMaxLines+1), After: "y\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := textLineDiff(tc.change); ok {
				t.Fatal("line diff applied")
			}
		})
	}
}

func TestTextLineDiffBoundsShownLines(t *testing.T) {
	base := numberedLines("old", 100)
	head := numberedLines("new", 100)
	d, ok := textLineDiff(domain.Change{Operation: "replace", Before: strings.Join(base, "\n"), After: strings.Join(head, "\n")})
	if !ok || d.added != 100 || d.removed != 100 || d.more != 200-textDiffShownLines {
		t.Fatalf("bound not applied: ok=%t %+v", ok, d)
	}
	changed := 0
	for _, line := range d.lines {
		if line[0] == '+' || line[0] == '-' {
			changed++
		}
	}
	if changed != textDiffShownLines {
		t.Fatalf("shown %d changed lines", changed)
	}
}

// policyChange is a multi-line JSON document stored as a string, the shape of
// an IAM policy, with one statement appended at head.
func policyChange() (string, string) {
	statement := func(sid string) string {
		return fmt.Sprintf("    {\n      \"Sid\": %q,\n      \"Effect\": \"Allow\",\n      \"Action\": [\"s3:GetObject\"],\n      \"Resource\": \"arn:aws:s3:::example-%s/*\"\n    }", sid, strings.ToLower(sid))
	}
	before := "{\n  \"Version\": \"2012-10-17\",\n  \"Statement\": [\n" + statement("Read") + "\n  ]\n}\n"
	after := "{\n  \"Version\": \"2012-10-17\",\n  \"Statement\": [\n" + statement("Read") + ",\n" + statement("BackupWrite") + "\n  ]\n}\n"
	return before, after
}

func TestTextChangesShowAddedLinesInEveryFormat(t *testing.T) {
	before, after := policyChange()
	r := tailChangeReport("a", "b")
	r.Applications[0].Resources[0].Changes = []domain.Change{{Path: "/spec/policyDocument", Operation: "replace", Before: before, After: after}}
	for _, format := range []string{"markdown", "pr-markdown", "terminal"} {
		data, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		// The old closing brace pairs with the new statement's closing brace,
		// so the change is six added lines and no removed line.
		for _, want := range []string{"/spec/policyDocument", "text changed", "6 added, 0 removed", `+      "Sid": "BackupWrite",`, `+    },`} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s lacks %q:\n%s", format, want, out)
			}
		}
		if strings.Contains(out, `\n`) {
			t.Fatalf("%s still shows the escaped string pair:\n%s", format, out)
		}
		if format != "terminal" && !strings.Contains(out, "~~~diff\n") {
			t.Fatalf("%s has no diff block:\n%s", format, out)
		}
	}
	data, err := Encode(r, "json", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	var decoded domain.Report
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if change := decoded.Applications[0].Resources[0].Changes[0]; change.Before != before || change.After != after {
		t.Fatal("JSON lost the full before/after values")
	}
}

// A text line can contain tildes, backticks and HTML. The block stays literal
// and the line cannot close it.
func TestTextDiffBlockContentStaysLiteral(t *testing.T) {
	r := tailChangeReport("a", "b")
	r.Applications[0].Resources[0].Changes = []domain.Change{{Path: "/data/script", Operation: "replace", Before: "keep\nold\n", After: "keep\n~~~~\n`a&b` <b>x</b>\u202e\n"}}
	for _, format := range []string{"markdown", "pr-markdown"} {
		data, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		for _, want := range []string{"~~~~~diff\n", "+~~~~\n", "+`a&b` <b>x</b>\\u202e\n"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s lacks %q:\n%s", format, want, out)
			}
		}
		if strings.Contains(out, "<code>a&b</code>") || strings.Contains(out, "\u202e") {
			t.Fatalf("%s rewrote or kept unsafe block content:\n%s", format, out)
		}
	}
}

func TestShortenedPRCommentClosesAnOpenDiffBlock(t *testing.T) {
	var b strings.Builder
	b.WriteString("intro\n  ~~~diff\n")
	for b.Len() < MaxPRMarkdownBytes+1000 {
		b.WriteString("  +" + strings.Repeat("x", 100) + "\n")
	}
	b.WriteString("  ~~~\nafter the block\n")
	out := string(boundPR([]byte(b.String())))
	fence := ""
	for _, line := range strings.SplitAfter(out, "\n") {
		fence, _ = textFence(fence, line)
	}
	if fence != "" || !strings.Contains(out, "Comment shortened to 60 KiB") || len(out) > MaxPRMarkdownBytes {
		t.Fatalf("shortened comment left a diff block open (fence %q) or exceeded the bound (%d bytes)", fence, len(out))
	}
}

// alertRule is an added list item, the shape of a Prometheus rule whose
// expression spans lines.
func alertRule() map[string]any {
	return map[string]any{
		"alert":       "BatchStalled",
		"expr":        "time() - max(\n  batch_last_success_timestamp_seconds\n) > 3 * 3600\n",
		"for":         "10m",
		"labels":      map[string]any{"severity": "warning"},
		"annotations": map[string]any{"summary": "The batch has not succeeded in three hours"},
	}
}

// TestAddedObjectsShowYAMLLines verifies that an added or removed object
// whose one-line JSON hides a multi-line string or would be cut is shown as
// YAML lines.
func TestAddedObjectsShowYAMLLines(t *testing.T) {
	d, ok := objectLines(domain.Change{Path: "/spec/groups/0/rules/3", Operation: "add", After: alertRule()})
	if !ok || d.object != "added" || d.added != len(d.lines) || d.removed != 0 {
		t.Fatalf("added rule not shown as lines: ok=%t %+v", ok, d)
	}
	got := strings.Join(d.lines, "\n")
	for _, want := range []string{"+alert: BatchStalled", "+expr: |", "+  time() - max(", "+    batch_last_success_timestamp_seconds", "+  ) > 3 * 3600", "+labels:", "+  severity: warning"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	removed, ok := objectLines(domain.Change{Path: "/spec/groups/0/rules/3", Operation: "remove", Before: alertRule()})
	if !ok || removed.object != "removed" || removed.removed != len(removed.lines) || !strings.HasPrefix(removed.lines[0], "-") {
		t.Fatalf("removed rule not shown as - lines: %+v", removed)
	}
}

func TestObjectLinesApplyOnlyToLongOrMultiLineObjects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change domain.Change
	}{
		{"short object", domain.Change{Path: "/eventPath", Operation: "add", After: map[string]any{"alerts": "off"}}},
		{"whole resource", domain.Change{Path: "/", Operation: "add", After: alertRule()}},
		{"redacted", domain.Change{Path: "/spec/x", Operation: "add", After: alertRule(), Redacted: true}},
		{"replace", domain.Change{Path: "/spec/x", Operation: "replace", Before: alertRule(), After: alertRule()}},
		{"scalar", domain.Change{Path: "/spec/x", Operation: "add", After: strings.Repeat("x", 500)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := objectLines(tc.change); ok {
				t.Fatal("object lines applied")
			}
		})
	}
	long := map[string]any{"args": []any{strings.Repeat("a", 250), strings.Repeat("b", 250)}}
	if _, ok := objectLines(domain.Change{Path: "/spec/x", Operation: "add", After: long}); !ok {
		t.Fatal("an object whose one-line JSON exceeds the PR width was not shown as lines")
	}
}

func TestAddedObjectLinesInEveryFormat(t *testing.T) {
	rule := alertRule()
	rule["labels"].(map[string]any)["priority"] = 3.0
	rule["env"] = []any{map[string]any{"name": "API_TOKEN", "value": "fictional-secret-value"}}
	r := tailChangeReport("a", "b")
	r.Applications[0].Resources[0].Changes = []domain.Change{{Path: "/spec/groups/0/rules/3", Operation: "add", After: rule, ItemNames: []string{"batch", "BatchStalled"}}}
	for _, format := range []string{"markdown", "pr-markdown", "terminal"} {
		data, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		for _, want := range []string{"added (", "+  time() - max(", "+  ) > 3 * 3600", "+  priority: 3\n"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s lacks %q:\n%s", format, want, out)
			}
		}
		if strings.Contains(out, "fictional-secret-value") || strings.Contains(out, `\n`) {
			t.Fatalf("%s exposed a sensitive value or kept the escaped one-line JSON:\n%s", format, out)
		}
	}
}

// Every format redacts the report again before display: a Secret
// reference stays visible and a literal value stays masked.
func TestSecretReferencesInEveryFormat(t *testing.T) {
	r := tailChangeReport("a", "b")
	r.Applications[0].Resources[0].Changes = []domain.Change{
		{Path: "/spec/template/spec/containers/0/env/7", Operation: "add", ItemNames: []string{"app", "SLACK_TOKEN"}, After: map[string]any{"name": "SLACK_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "app-secrets", "key": "slack-token"}}}},
		{Path: "/spec/template/spec/containers/0/env/8", Operation: "add", ItemNames: []string{"app", "API_TOKEN"}, After: map[string]any{"name": "API_TOKEN", "value": "fictional-literal"}},
	}
	for _, format := range []string{"json", "markdown", "pr-markdown", "terminal"} {
		data, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		if !strings.Contains(out, "app-secrets") || !strings.Contains(out, "slack-token") || strings.Contains(out, "fictional-literal") {
			t.Fatalf("%s hid the reference or exposed the value:\n%s", format, out)
		}
	}
}

// A credential in URL userinfo is masked in change values, line hunks and
// Application source fields in every format; the host stays.
func TestURLUserinfoCredentialsAreMaskedInEveryFormat(t *testing.T) {
	r := tailChangeReport("--db=postgres://app:fictional-pass-a@db.example:5432/app", "--db=postgres://app:fictional-pass-b@db.example:5432/app")
	r.Applications[0].Resources[0].LineHunks = []domain.LineHunk{{Path: "/spec/template/spec/containers[app]/args", BaseStart: 3, HeadStart: 3, Lines: []string{
		"-- --db=postgres://app:fictional-pass-a@db.example:5432/app", "+- --db=postgres://app:fictional-pass-b@db.example:5432/app",
	}}}
	r.Applications[0].Resources = append(r.Applications[0].Resources, domain.ResourceDiff{
		ID: domain.ResourceID{Group: "argoproj.io", Kind: "Application", Namespace: "argocd", Name: "child"}, State: "changed", Comparison: "complete",
		Changes: []domain.Change{{Path: "/spec/source/repoURL", Operation: "replace", Before: "https://x-access-token:fictional-pat-a@git.example/charts.git", After: "https://fictional-pat-b@git.example/charts.git"}},
	})
	for _, format := range Formats {
		data, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		if strings.Contains(out, "fictional-") {
			t.Fatalf("%s exposed a URL credential:\n%s", format, out)
		}
		for _, want := range []string{"db.example:5432/app", "git.example/charts.git"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s lost the URL host %q:\n%s", format, want, out)
			}
		}
	}
}

// A change value is redacted at the change's own path, not at its position
// in the report, so references whose context is in the path stay visible.
func TestChangeValuesAreRedactedAtTheirOwnPath(t *testing.T) {
	r := tailChangeReport("a", "b")
	r.Applications[0].Resources[0].Changes = []domain.Change{
		{Path: "/spec/data/1", Operation: "add", ItemNames: []string{"SLACK_TOKEN"}, After: map[string]any{"secretKey": "slack-secret-key", "remoteRef": map[string]any{"key": "app", "property": "slack"}}},
		{Path: "/spec/tls/0", Operation: "add", After: map[string]any{"hosts": []any{"app.example.test"}, "secretName": "app-cert"}},
		{Path: "/spec/apiToken", Operation: "replace", Before: "fictional-old", After: "fictional-new"},
		{Path: "/spec/accessToken", Operation: "add", After: map[string]any{"token": "fictional-token"}},
	}
	for _, format := range []string{"json", "markdown", "pr-markdown", "terminal"} {
		data, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		for _, want := range []string{"slack-secret-key", "app-cert"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s hid %q:\n%s", format, want, out)
			}
		}
		if strings.Contains(out, "fictional-") {
			t.Fatalf("%s exposed a value:\n%s", format, out)
		}
		if format == "json" {
			var decoded domain.Report
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			if added := decoded.Applications[0].Resources[0].Changes[3]; added.Before != nil || added.After == nil {
				t.Fatalf("an added sensitive value gained a masked before: %+v", added)
			}
		}
		if format != "json" && strings.Contains(out, "/spec/accessToken`: value changed (hidden)") {
			t.Fatalf("%s shows an added value as a hidden change:\n%s", format, out)
		}
	}
}

// Fields not compared because they differed between two renders of identical
// inputs are named in every format, for changed and unknown resources.
func TestUnknownPathsInEveryFormat(t *testing.T) {
	r := tailChangeReport("a", "b")
	app := &r.Applications[0]
	app.Resources[0].Comparison = "incomplete"
	app.Resources[0].UnknownPaths = []string{"/spec/template/metadata/annotations/checksum"}
	app.Resources = append(app.Resources, domain.ResourceDiff{ID: domain.ResourceID{Kind: "Secret", Namespace: "demo", Name: "db"}, State: "unknown", Comparison: "incomplete", Changes: []domain.Change{}, UnknownPaths: []string{"/data/password"}})
	for _, format := range []string{"json", "markdown", "pr-markdown", "terminal"} {
		data, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		out := string(data)
		for _, want := range []string{"/spec/template/metadata/annotations/checksum", "/data/password"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s does not name %s:\n%s", format, want, out)
			}
		}
		if format != "json" && strings.Count(out, "differs between two renders of identical inputs") < 2 {
			t.Fatalf("%s does not explain why the fields were not compared:\n%s", format, out)
		}
		// The PR comment names them next to the visible target change.
		if visible, _, _ := strings.Cut(out, "<details>"); format == "pr-markdown" && !strings.Contains(visible, "/spec/template/metadata/annotations/checksum") {
			t.Fatalf("PR target changes do not name the field:\n%s", out)
		}
	}
}
