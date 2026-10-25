package reporter

import (
	"bytes"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

func TestPolicyEvidenceRedactionAndPRBounds(t *testing.T) {
	r := domain.NewReport()
	r.Status = "policy_violation"
	r.Revisions.Head = strings.Repeat("b", 40)
	r.Policy = &domain.PolicySummary{Evaluation: "complete", Scope: "explicit-applications", Violations: []domain.PolicyViolation{}}
	for i := 0; i < 256; i++ {
		r.Policy.Violations = append(r.Policy.Violations, domain.PolicyViolation{
			PolicyID: "keep-services", RuleID: "deny-resource-removal-v1",
			Application: domain.ApplicationID{Instance: "test", Name: "fictional-sensitive-token"},
			Resource:    domain.ResourceID{Kind: "Service", Name: "fictional-sensitive-token"},
			Base:        domain.PolicyRevision{Revision: "fictional-sensitive-token"},
			Reason:      strings.Repeat("<untrusted> ", 1000), Action: "Review the trusted profile",
		})
		r.PolicyViolations = append(r.PolicyViolations, "fictional-sensitive-token")
	}
	for _, format := range []string{"json", "markdown", "pr-markdown"} {
		data, err := Encode(r, format, security.Redactor{Secrets: []string{"fictional-sensitive-token"}})
		if err != nil || bytes.Contains(data, []byte("fictional-sensitive-token")) {
			t.Fatalf("policy evidence leaked in %s: %v", format, err)
		}
		if format == "pr-markdown" {
			if len(data) > MaxPRMarkdownBytes || bytes.Contains(data, []byte("<untrusted>")) || !bytes.Contains(data, []byte(r.Revisions.Head)) {
				t.Fatal("policy PR bounds, escaping or head identity lost")
			}
		}
	}
}
