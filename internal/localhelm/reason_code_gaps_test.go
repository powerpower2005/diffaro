package localhelm

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

// g67bReasonCode returns the reason code of a render failure and checks that
// the JSON form of its issue carries it.
func g67bReasonCode(t *testing.T, err error) string {
	t.Helper()
	var failure *domain.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("not a structured failure: %v", err)
	}
	data, jsonErr := json.Marshal(failure.Issue)
	if jsonErr != nil {
		t.Fatal(jsonErr)
	}
	if failure.Issue.ReasonCode != "" && !strings.Contains(string(data), `"reasonCode":"`+failure.Issue.ReasonCode+`"`) {
		t.Fatalf("reason code missing from JSON issue: %s", data)
	}
	return failure.Issue.ReasonCode
}

// A missing chart directory and a chart without Chart.yaml have distinct
// reason codes, so reports title them by cause.
func TestMissingChartPathReasonCodes(t *testing.T) {
	for _, tc := range []struct {
		name, root, want string
		extra            map[string][]byte
	}{
		{"absent directory", "missing", "chart_path_missing", nil},
		{"directory without Chart.yaml", "docs", "chart_metadata_missing", map[string][]byte{"docs/README.md": []byte("notes")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := fixtureRequest()
			for name, data := range tc.extra {
				req.Repositories[testRepo+"@"+testSHA][name] = data
			}
			req.Plan[0].Source["path"] = tc.root
			_, err := Render(req)
			if got := g67bReasonCode(t, err); got != tc.want {
				t.Fatalf("reasonCode=%q want %q", got, tc.want)
			}
		})
	}
}

// A template failure with a safe location carries template_failed.
func TestTemplateFailureReasonCode(t *testing.T) {
	req := fixtureRequest()
	files := req.Repositories[testRepo+"@"+testSHA]
	files["app/values.yaml"] = []byte("secret: fictional-secret-value\n")
	files["app/templates/deployment.yaml"] = []byte("a: 1\n{{ fail .Values.secret }}\n")
	_, err := Render(req)
	if got := g67bReasonCode(t, err); got != "template_failed" {
		t.Fatalf("reasonCode=%q want template_failed: %v", got, err)
	}
}
