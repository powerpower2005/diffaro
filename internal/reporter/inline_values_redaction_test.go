package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// The final report pass redacts an inline value change at its full pointer
// in the Application spec, so inline values stay masked whole even when a
// change reaches the report unmasked.
func TestInlineValueChangesStayMaskedInTheReportPass(t *testing.T) {
	for _, pointer := range []string{"/spec/source/helm/values", "/spec/sources/1/helm/values"} {
		r := inputChangeReport()
		r.Applications[0].InputChanges = []domain.InputChange{{Kind: "inlineValues", Path: pointer, State: "changed", Comparison: "complete", Changes: []domain.Change{
			{Path: "/image/tag", Operation: "replace", Before: "fictional-v1", After: "fictional-v2"},
		}}}
		for _, format := range []string{"json", "markdown", "pr-markdown", "terminal"} {
			data, err := Encode(r, format, security.Redactor{})
			if err != nil {
				t.Fatal(err)
			}
			if out := string(data); strings.Contains(out, "fictional-") || !strings.Contains(out, "/image/tag") {
				t.Fatalf("%s at %s exposed an inline value or lost its key path:\n%s", format, pointer, out)
			}
		}
	}
}
