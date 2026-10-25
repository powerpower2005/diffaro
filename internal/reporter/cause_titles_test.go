package reporter

import (
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// The Errors section names each cause by its reason code, not by
// the broad issue code: a missing value file reads "Value file does not
// exist", not "Local render failed".
func TestErrorsSectionTitlesCauseByReasonCode(t *testing.T) {
	for _, tc := range []struct{ code, reasonCode, title string }{
		{"local_render_failed", "chart_path_missing", "Chart path does not exist"},
		{"not_found", "chart_path_missing", "Chart path does not exist"},
		{"local_render_failed", "chart_metadata_missing", "Chart.yaml is missing"},
		{"local_render_failed", "value_file_missing", "Value file does not exist"},
		{"local_render_failed", "template_failed", "Helm template failed"},
		{"local_render_failed", "parameter_invalid", "Helm parameter name is invalid"},
		{"local_render_failed", "rendered_output_invalid", "Rendered output is invalid"},
		{"local_render_failed", "input_invalid", "Helm input is invalid"},
		{"not_found", "revision_missing", "Git revision does not exist"},
		{"backend_error", "server_error", "Backend server error"},
		{"backend_error", "unexpected_status", "Unexpected backend response"},
		{"access_denied", "credential_missing", "Credential is missing"},
		{"access_denied", "authentication_rejected", "Authentication was rejected"},
		{"access_denied", "request_forbidden", "Request was forbidden"},
		{"timeout", "timeout", "Timed out"},
	} {
		t.Run(tc.code+"/"+tc.reasonCode, func(t *testing.T) {
			r := domain.NewReport()
			r.Status = "partial_failure"
			app := domain.ApplicationResult{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}, State: "failed", Execution: "failed"}
			app.Issues = []domain.Issue{{Code: tc.code, ReasonCode: tc.reasonCode, Stage: "render", Side: "head", Application: &app.ID, Reason: "fictional reason", Action: "fictional action"}}
			r.Applications = []domain.ApplicationResult{app}
			fallback := strings.ToUpper(tc.code[:1]) + strings.ReplaceAll(tc.code[1:], "_", " ")
			for _, format := range []string{"markdown", "pr-markdown"} {
				data, err := Encode(r, format, security.Redactor{})
				if err != nil {
					t.Fatal(err)
				}
				out := string(data)
				if !strings.Contains(out, "### "+tc.title+" (`"+tc.code+"` · `"+tc.reasonCode+"`)") {
					t.Fatalf("%s lacks cause title %q:\n%s", format, tc.title, out)
				}
				if fallback != tc.title && strings.Contains(out, "### "+fallback+" (") {
					t.Fatalf("%s titled the cause by its issue code:\n%s", format, out)
				}
			}
		})
	}
}
