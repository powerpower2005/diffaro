package cli

import (
	"bytes"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// Stderr diagnostics show an internal_error's site and cause chain, and only
// for internal_error.
func TestStderrShowsDefectSiteAndChainOnly(t *testing.T) {
	diagnostic := &domain.Diagnostic{Site: "fictional/defect.go:12", Chain: []string{"*fictional.Error", "io.EOF"}}
	var out bytes.Buffer
	printIssue(&out, domain.Issue{Code: domain.CodeInternalError, ReasonCode: domain.ReasonUnclassified, Stage: "render", Reason: "preview could not complete", Action: domain.DefectAction, Diagnostic: diagnostic}, security.Redactor{})
	if !strings.Contains(out.String(), "Site: fictional/defect.go:12") || !strings.Contains(out.String(), "Cause chain: *fictional.Error > io.EOF") {
		t.Fatalf("defect diagnostic missing:\n%s", out.String())
	}
	out.Reset()
	printIssue(&out, domain.Issue{Code: domain.CodeNotFound, Stage: "lookup", Reason: "missing", Action: "check", Diagnostic: diagnostic}, security.Redactor{})
	if strings.Contains(out.String(), "fictional/defect.go") {
		t.Fatalf("classified failure shows its diagnostic:\n%s", out.String())
	}
}
