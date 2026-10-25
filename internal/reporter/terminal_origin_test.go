package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// The terminal report prints each failed Application's origin right after
// the Application line, not only in the verdict counts.
func TestTerminalPrintsFailureOriginNextToApplication(t *testing.T) {
	for _, origin := range []string{"preexisting", "introduced", "transient"} {
		t.Run(origin, func(t *testing.T) {
			base := "failed"
			if origin != "preexisting" {
				base = "complete"
			}
			r := failureReport(func() (domain.ApplicationResult, []domain.Issue) {
				return originFailure("old", origin, "not_found", "chart_path_missing", base, "failed")
			})
			data, err := Encode(r, "terminal", security.Redactor{})
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(data), "\n")
			for i, line := range lines {
				if !strings.Contains(line, "App east/argocd/old (") {
					continue
				}
				if i+1 >= len(lines) || lines[i+1] != "  Failure origin: "+originLabels[domain.FailureOrigin(origin)] {
					t.Fatalf("no failure origin after the Application line:\n%s", data)
				}
				return
			}
			t.Fatalf("Application line missing:\n%s", data)
		})
	}
}
