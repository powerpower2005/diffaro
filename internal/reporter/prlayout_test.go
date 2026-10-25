package reporter

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// layoutReport has one changed, one failed, one skipped, one unchanged and
// one input-only Application.
func layoutReport() domain.Report {
	r := tailChangeReport("a", "b")
	r.Status = "partial_failure"
	r.Revisions = domain.RevisionPair{Repository: "example/charts", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40)}
	id := func(name string) domain.ApplicationID {
		return domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: name}
	}
	failed := id("failed")
	r.Applications = append(r.Applications,
		domain.ApplicationResult{ID: failed, State: "unknown", Execution: "failed", Issues: []domain.Issue{{Code: "render_failed", Stage: "render", Reason: "template failed", Action: "fix the template", Application: &failed}}},
		domain.ApplicationResult{ID: id("skipped"), State: "unknown", Execution: "skipped", SkipReason: "missing destination capabilities"},
		domain.ApplicationResult{ID: id("quiet"), State: "unchanged", Execution: "complete"},
		domain.ApplicationResult{ID: id("inputs"), State: "unchanged", Execution: "complete", InputChanges: []domain.InputChange{{Kind: "valueFile", Path: "values.yaml", State: "changed", Comparison: "complete"}}},
	)
	return r
}

func TestPRHeadlineCountsChangesAndNamesUnknownCoverage(t *testing.T) {
	out := encodeText(t, layoutReport(), "pr-markdown")
	headline := strings.SplitN(out, "\n\n", 3)[:2]
	text := strings.Join(headline, "\n")
	for _, want := range []string{"## Diffaro: 1 resource change in 1 Application (1 changed)", "base `aaaaaaa` → head `bbbbbbb`", "3 Applications compared", "1 failed, 1 skipped: their changes are unknown, not unchanged"} {
		if !strings.Contains(text, want) {
			t.Fatalf("headline lacks %q:\n%s", want, out)
		}
	}
	// The full SHAs stay in the marker comment and the details.
	if strings.Count(out, strings.Repeat("b", 40)) < 2 {
		t.Fatalf("full head SHA is missing:\n%s", out)
	}
}

func TestPRNamesApplicationsWithoutChanges(t *testing.T) {
	out := encodeText(t, layoutReport(), "pr-markdown")
	visible := outsideDetails(out)
	for _, want := range []string{
		"**Not compared, changes unknown (2):** `east/argocd/failed` (failed, see Needs attention), `east/argocd/skipped` (skipped: missing destination capabilities)",
		"**PR inputs changed, rendered targets unchanged (1):** `east/argocd/inputs`",
		"**No target changes (1):** `east/argocd/quiet`",
	} {
		if !strings.Contains(visible, want) {
			t.Fatalf("visible part lacks %q:\n%s", want, visible)
		}
	}
	if strings.Contains(visible, "### `quiet`") || strings.Contains(visible, "### `failed`") {
		t.Fatalf("an Application without changes got its own section:\n%s", visible)
	}
}

// A run that compares no Application, such as a PR no readable Application
// reads, prints no empty Applications table or Render context heading.
func TestPROmitsEmptyApplicationSections(t *testing.T) {
	r := domain.NewReport()
	r.Status = "success"
	r.Revisions = domain.RevisionPair{Repository: "example/charts", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40)}
	out := encodeText(t, r, "pr-markdown")
	for _, absent := range []string{"#### Applications", "| Application |", "#### Render context"} {
		if strings.Contains(out, absent) {
			t.Fatalf("empty section %q is printed:\n%s", absent, out)
		}
	}
	if !strings.Contains(out, "0 Applications compared") {
		t.Fatalf("the headline must still state that no Application was compared:\n%s", out)
	}
	with := encodeText(t, layoutReport(), "pr-markdown")
	for _, present := range []string{"#### Applications", "#### Render context"} {
		if !strings.Contains(with, present) {
			t.Fatalf("section %q is missing when Applications were compared:\n%s", present, with)
		}
	}
}

// A value holding a tilde run or HTML cannot close its diff block or render
// as markup.
func TestPRDiffBlockCannotBeClosedByAValue(t *testing.T) {
	r := tailChangeReport("plain", "~~~~~~\n</details><script>x</script>\n~~~")
	out := encodeText(t, r, "pr-markdown")
	outside := outsideFences(out)
	if strings.Contains(outside, "<script>") || strings.Contains(outside, "~~~~~~\n") {
		t.Fatalf("a value escaped its diff block:\n%s", out)
	}
	if !strings.Contains(out, "~~~~~~~diff\n") || !strings.Contains(out, "+</details><script>x</script>\n") {
		t.Fatalf("diff block lost the value or used a short fence:\n%s", out)
	}
	// The value's </details> is inside the fence; the comment's own tags pair.
	if strings.Count(outside, "<details>") != strings.Count(outside, "</details>") {
		t.Fatalf("details tags do not pair outside the value:\n%s", out)
	}
}

func TestPRLongManifestIsCollapsedWithItsLineCount(t *testing.T) {
	manifest := func(n int) map[string]any {
		data := map[string]any{}
		for i := 0; i < n; i++ {
			data[fmt.Sprintf("key-%02d", i)] = "value"
		}
		return map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "data": data}
	}
	r := tailChangeReport("a", "b")
	r.Applications[0].Resources = []domain.ResourceDiff{
		{ID: domain.ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: "short"}, State: "added", Comparison: "complete", Changes: []domain.Change{{Path: "/", Operation: "add", After: manifest(10)}}},
		{ID: domain.ResourceID{Kind: "ConfigMap", Namespace: "demo", Name: "long"}, State: "removed", Comparison: "complete", Changes: []domain.Change{{Path: "/", Operation: "remove", Before: manifest(30)}}},
	}
	out, err := Encode(r, "pr-markdown", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	short := text[strings.Index(text, "`demo/short`"):strings.Index(text, "`demo/long`")]
	if strings.Contains(short, "<details>") || !strings.Contains(short, "+  key-09: value\n") {
		t.Fatalf("a short manifest is not shown inline:\n%s", short)
	}
	long := text[strings.Index(text, "`demo/long`"):]
	if !strings.Contains(long, "<details>\n<summary>removed manifest (33 lines)</summary>") || !strings.Contains(long, "-  key-00: value\n") {
		t.Fatalf("a long manifest is not collapsed with its line count:\n%s", long)
	}
}

func TestPRFieldChangesAreBoundedPerResource(t *testing.T) {
	r := tailChangeReport("a", "b")
	var changes []domain.Change
	for i := 0; i < 25; i++ {
		changes = append(changes, domain.Change{Path: fmt.Sprintf("/data/key-%02d", i), Operation: "replace", Before: "old", After: "new"})
	}
	r.Applications[0].Resources[0].Changes = changes
	out := encodeText(t, r, "pr-markdown")
	if !strings.Contains(out, "@@ /data/key-19 @@") || strings.Contains(out, "@@ /data/key-20 @@") || !strings.Contains(out, " … 5 more field change(s) in JSON or the full Markdown report\n") {
		t.Fatalf("field changes past 20 are shown or not counted:\n%s", out)
	}
}

func TestPRHeadlineOfAFailedRunSaysChangesAreUnknown(t *testing.T) {
	for status, label := range map[string]string{"failed": "failed", "denied": "access denied", "invalid_configuration": "invalid configuration", "cancelled": "cancelled"} {
		r := domain.NewReport()
		r.Status = domain.Status(status)
		out := encodeText(t, r, "pr-markdown")
		if !strings.Contains(out, "## Diffaro: "+label+", target changes are unknown\n") || strings.Contains(out, "no target changes") {
			t.Fatalf("%s run headline does not say changes are unknown:\n%s", status, out)
		}
	}
}

// A resource with line hunks shows them with their line numbers and paths
// instead of its field changes.
func TestPRShowsLineHunksAndHiddenChanges(t *testing.T) {
	r := tailChangeReport("a", "b")
	res := &r.Applications[0].Resources[0]
	long := strings.Repeat("x", 400)
	res.Changes = append(res.Changes, domain.Change{Path: "/spec/token", Operation: "replace", Before: security.Mask, After: security.Mask, Redacted: true})
	res.LineHunks = []domain.LineHunk{{Path: "/spec/template/spec/containers[app]/env[B]", BaseStart: 11, HeadStart: 11, Lines: []string{"   - name: A", "+  - name: B", "-  value: " + long + "-old", "+  value: " + long + "-new", "   - name: C"}}}
	out := encodeText(t, r, "pr-markdown")
	// Hidden changes are inside the hunks (manifest marks them), so the
	// field change is not repeated after them.
	for _, want := range []string{"@@ -11 +11 @@ /spec/template/spec/containers[app]/env[B]\n   - name: A\n+  - name: B\n", "-old\n", "-new\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("PR Markdown lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "@@ /spec/template/spec/containers/0/args @@") || strings.Contains(out, "/spec/token") {
		t.Fatalf("field changes were shown next to the hunks:\n%s", out)
	}
}

// outsideDetails drops collapsed <details> blocks, leaving what a reader
// sees without expanding anything.
func outsideDetails(markdown string) string {
	var b strings.Builder
	depth := 0
	for _, line := range strings.SplitAfter(outsideFences(markdown), "\n") {
		switch {
		case strings.HasPrefix(line, "<details>"):
			depth++
		case strings.HasPrefix(line, "</details>"):
			depth--
		case depth == 0:
			b.WriteString(line)
		}
	}
	return b.String()
}

// Failures read as one line per cause with its origin and Applications;
// where, why and what to do stay in collapsed details. A run-level failure
// shows its reason, because nothing else explains the empty result.
func TestPRErrorsSummarizeAndKeepDetailsCollapsed(t *testing.T) {
	r := layoutReport()
	r.Applications[1].FailureOrigin = "introduced"
	out := encodeText(t, r, "pr-markdown")
	visible := outsideDetails(out)
	for _, want := range []string{"## Needs attention: 1 Application not compared", "- **Render failed** (introduced by this PR): `east/argocd/failed`", "Their resource changes are unknown, not unchanged."} {
		if !strings.Contains(visible, want) {
			t.Fatalf("visible part lacks %q:\n%s", want, visible)
		}
	}
	if strings.Contains(visible, "fix the template") || strings.Contains(visible, "**Why:**") || strings.Contains(visible, "Counted failures") || strings.Contains(visible, "Comparison is incomplete") {
		t.Fatalf("error details are not collapsed:\n%s", visible)
	}
	details := out[strings.Index(out, "<summary>Error details"):]
	for _, want := range []string{"**Why:** template failed", "**Action:** fix the template", "stage `render`"} {
		if !strings.Contains(details, want) {
			t.Fatalf("error details lack %q:\n%s", want, details)
		}
	}
	run := domain.NewReport()
	run.Status = "failed"
	run.Issues = []domain.Issue{{Code: "rate_limited", Stage: "connection", Reason: "API rate limit was reached", Action: "wait for the quota reset"}}
	visible = outsideDetails(encodeText(t, run, "pr-markdown"))
	if !strings.Contains(visible, "## Needs attention: the run did not complete") || !strings.Contains(visible, "- **Rate limited**: API rate limit was reached") || strings.Contains(visible, "wait for the quota reset") {
		t.Fatalf("a run-level failure does not show its reason or shows its details:\n%s", visible)
	}
}

// A cause line names up to prMaxCauseApps Applications and counts the other
// Applications, whether a run-level issue with the same cause comes before
// or after them. Preview lists Application issues at report level too.
func TestPRCauseLineNamesFiveApplicationsBesideARunLevelIssue(t *testing.T) {
	cause := domain.Issue{Code: "render_failed", Stage: "render", Reason: "template failed", Action: "fix the template"}
	for _, runFirst := range []bool{true, false} {
		r := domain.NewReport()
		r.Status = "partial_failure"
		if runFirst {
			r.Issues = append(r.Issues, cause)
		}
		var names []string
		for i := 1; i <= 7; i++ {
			id := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: fmt.Sprintf("app-%d", i)}
			issue := cause
			issue.Application = &id
			r.Applications = append(r.Applications, domain.ApplicationResult{ID: id, State: "unknown", Execution: "failed", Issues: []domain.Issue{issue}})
			r.Issues = append(r.Issues, issue)
			names = append(names, "`"+id.String()+"`")
		}
		if !runFirst {
			r.Issues = append(r.Issues, cause)
		}
		visible := outsideDetails(encodeText(t, r, "pr-markdown"))
		want := "- **Render failed**: " + strings.Join(names[:prMaxCauseApps], ", ") + ", 2 more\n"
		if !strings.Contains(visible, want) {
			t.Fatalf("run-level issue first=%t: cause line is not %q:\n%s", runFirst, want, visible)
		}
	}
}

// Every in-page link resolves to an anchor in the same comment, even when
// more failed Applications than the render-context bound sort before the
// changed one.
func TestPRLinksResolveToAnchorsPastTheFailureBound(t *testing.T) {
	r := tailChangeReport("a", "b")
	r.Status = "partial_failure"
	r.Applications[0].ID = domain.ApplicationID{Instance: "zz", Namespace: "argocd", Name: "api"}
	for i := 0; i < 31; i++ {
		id := domain.ApplicationID{Instance: "aa", Namespace: "argocd", Name: fmt.Sprintf("failed-%02d", i)}
		r.Applications = append(r.Applications, domain.ApplicationResult{ID: id, State: "unknown", Execution: "failed", Issues: []domain.Issue{{Code: "render_failed", Stage: "render", Reason: "template failed", Application: &id}}})
	}
	out := encodeText(t, r, "pr-markdown")
	links := regexp.MustCompile(`\]\(#([^)]+)\)`).FindAllStringSubmatch(out, -1)
	if len(links) == 0 {
		t.Fatalf("the changed Application has no render context link:\n%s", out)
	}
	for _, link := range links {
		if !strings.Contains(out, `<a id="`+link[1]+`"></a>`) {
			t.Fatalf("link #%s has no anchor:\n%s", link[1], out)
		}
	}
}

// The default action of a fully compared changed Application repeats what
// its changes already say; no format prints it, while specific actions such
// as resolving a failure stay.
func TestDefaultReviewActionIsNotRepeated(t *testing.T) {
	r := layoutReport()
	for _, format := range []string{"pr-markdown", "markdown", "terminal"} {
		out := encodeText(t, r, format)
		if strings.Contains(out, defaultReviewAction) || strings.Contains(strings.ReplaceAll(out, "&#39;", "'"), unchangedAction) {
			t.Fatalf("%s repeats the default action:\n%s", format, out)
		}
		if format != "pr-markdown" && !strings.Contains(out, "Resolve the skip reason and rerun") {
			t.Fatalf("%s lost a specific action:\n%s", format, out)
		}
	}
	if !strings.Contains(encodeText(t, r, "json"), "\"state\": \"changed\"") {
		t.Fatal("fixture lost its changed Application")
	}
}
