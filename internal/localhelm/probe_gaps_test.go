package localhelm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

// The probe renders the same chart, values and render context.
// Application values, not only chart defaults, decide whether a time call
// is reached.
func TestExternalProbeUsesApplicationValues(t *testing.T) {
	e := fakeExternal(t)
	for _, tc := range []struct {
		name     string
		defaults string
		useNow   bool
		skips    bool
	}{
		{"Application switches a default call off", "useNow: true\n", false, false},
		{"Application switches a default-off call on", "useNow: false\n", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := fixtureRequest()
			files := req.Repositories[testRepo+"@"+testSHA]
			files["app/values.yaml"] = append(files["app/values.yaml"], tc.defaults...)
			files["app/templates/config.yaml"] = []byte(`{{ if .Values.useNow }}{{ now }}{{ end }}`)
			req.Plan[0].Source["helm"].(map[string]any)["valuesObject"] = map[string]any{"useNow": tc.useNow}
			_, err := e.Render(context.Background(), req)
			var skipped *domain.RenderSkipped
			if got := errors.As(err, &skipped); got != tc.skips {
				t.Fatalf("skipped=%t (err %v), want %t", got, err, tc.skips)
			}
			if tc.skips && !strings.Contains(skipped.Reason, "template app/templates/config.yaml:1 invokes randomness") {
				t.Fatal("skip does not name the calling template", skipped.Reason)
			}
		})
	}
}

// When the probe calls a function, the reason is "external Helm
// template <file>:<line> invokes ...", not the could-not-rule-out reason.
func TestExternalProbeCallKeepsNondeterministicReason(t *testing.T) {
	e := fakeExternal(t)
	req := fixtureRequest()
	req.Repositories[testRepo+"@"+testSHA]["app/templates/config.yaml"] = []byte(`# {{ now }}`)
	_, err := e.Render(context.Background(), req)
	var skipped *domain.RenderSkipped
	if !errors.As(err, &skipped) {
		t.Fatal("active time call not skipped", err)
	}
	reason := strings.TrimPrefix(skipped.Reason, "Local Helm analysis skipped: ")
	if !strings.HasPrefix(reason, "external Helm template app/templates/config.yaml:1 invokes randomness") {
		t.Fatal("a proven call lost its location reason", skipped.Reason)
	}
	if strings.Contains(reason, "could not render the chart to rule out a call") {
		t.Fatal("a proven call was reported as an unrenderable chart", skipped.Reason)
	}
}
