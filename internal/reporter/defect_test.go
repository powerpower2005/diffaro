package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// Every human-readable format shows an internal_error's diagnostic, which a
// bug report needs, and no format shows the diagnostic of a classified
// failure, whose code and reason already name the cause.
func TestDefectDiagnosticsAppearOnlyForInternalErrors(t *testing.T) {
	id := domain.ApplicationID{Instance: "fictional", Namespace: "argocd", Name: "app"}
	defect := domain.Issue{Code: domain.CodeInternalError, ReasonCode: domain.ReasonUnclassified, Stage: "render", Reason: "preview could not complete", Action: domain.DefectAction, Application: &id, Side: "head",
		Diagnostic: &domain.Diagnostic{Site: "fictional/defect.go:12", Chain: []string{"*fictional.Error", "io.ErrUnexpectedEOF"}}}
	classified := domain.Issue{Code: domain.CodeNotFound, Stage: "lookup", Reason: "missing", Action: "check", Application: &id, Side: "base",
		Diagnostic: &domain.Diagnostic{Site: "fictional/classified.go:34", HTTPStatus: 404}}
	report := domain.NewReport()
	report.Status = domain.StatusFailed
	report.Applications = []domain.ApplicationResult{{ID: id, State: domain.StateUnknown, Execution: domain.ExecutionFailed, Issues: []domain.Issue{defect, classified}}}
	report.Issues = []domain.Issue{defect, classified}
	for _, format := range []string{"pr-markdown", "markdown", "terminal"} {
		out, err := Encode(report, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		text := string(out)
		if !strings.Contains(text, "fictional/defect.go:12") || !strings.Contains(text, "fictional.Error") || !strings.Contains(text, "io.ErrUnexpectedEOF") {
			t.Fatalf("%s lacks the defect diagnostic:\n%s", format, text)
		}
		if strings.Contains(text, "fictional/classified.go:34") {
			t.Fatalf("%s shows a classified failure's diagnostic:\n%s", format, text)
		}
	}
}

// The full Markdown Application details repeat each issue with its defect
// diagnostic.
func TestFullMarkdownIssueShowsDefectDiagnostic(t *testing.T) {
	var b strings.Builder
	writeIssue(&b, domain.Issue{Code: domain.CodeInternalError, Stage: "render", Reason: "r", Action: domain.DefectAction, Diagnostic: &domain.Diagnostic{Site: "fictional/defect.go:12"}}, "")
	if !strings.Contains(b.String(), "fictional/defect.go:12") {
		t.Fatalf("issue lacks its diagnostic:\n%s", b.String())
	}
}
