package reporter

import (
	"fmt"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

// The verdict names at most 3 skipped Applications, then counts
// the rest.
func TestVerdictNamesAtMostThreeSkipped(t *testing.T) {
	r := domain.NewReport()
	r.Status = "success"
	for i := 1; i <= 5; i++ {
		r.Applications = append(r.Applications, domain.ApplicationResult{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: fmt.Sprintf("skipped-%d", i)}, State: "unknown", Execution: "skipped", SkipReason: "helper function unavailable"})
	}
	var line string
	for _, l := range verdictLines(r) {
		if strings.Contains(l, "Not analyzed:") {
			line = l
		}
	}
	if !strings.Contains(line, "Not analyzed: 5 Application(s) skipped") {
		t.Fatalf("verdict does not count every skipped Application: %q", line)
	}
	if got := strings.Count(line, "east/argocd/skipped-"); got != 3 {
		t.Fatalf("verdict names %d skipped Applications, want 3: %q", got, line)
	}
	if !strings.Contains(line, "2 more in the report") {
		t.Fatalf("verdict does not count the skipped Applications past 3: %q", line)
	}
}

// The inputs-changed failure origin is explained in every human format.
func TestInputsChangedOriginIsLabeled(t *testing.T) {
	r := focusReport()
	for i := range r.Applications {
		if r.Applications[i].Execution == "failed" {
			r.Applications[i].FailureOrigin = "inputs-changed"
		}
	}
	label := "already failing, inputs changed by this PR"
	for _, format := range []string{"markdown", "pr-markdown", "terminal"} {
		out := encodeText(t, r, format)
		if !strings.Contains(out, label) {
			t.Fatalf("%s does not explain the inputs-changed origin:\n%s", format, out)
		}
	}
}
