package preview

import (
	"testing"

	"diffaro/internal/domain"
)

func TestSelectionFailuresWithholdOnlyAffectedInstanceEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, instance string
		global         bool
	}{
		{"instance one", "one", false}, {"instance two", "two", false}, {"global", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := domain.NewReport()
			r.Applications = []domain.ApplicationResult{{ID: domain.ApplicationID{Instance: "one", Name: "api"}, Execution: "complete"}, {ID: domain.ApplicationID{Instance: "two", Name: "api"}, Execution: "complete"}}
			r.Graph = &domain.GraphSummary{BaseComplete: true, HeadComplete: true}
			issue := domain.Issue{Code: "selection_changed", Application: &domain.ApplicationID{Instance: tc.instance}}
			if tc.global {
				issue.Application = nil
			}
			ApplyRunIssues(&r, []domain.Issue{issue})
			if r.Status != "partial_failure" || r.Graph.BaseComplete || r.Graph.HeadComplete || len(r.Applications) != 2 {
				t.Fatal("selection failure destroyed independent results or advertised complete coverage")
			}
		})
	}
}
