package localhelm

import (
	"errors"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/helmerror"

	"helm.sh/helm/v4/pkg/chart/common"
	chart "helm.sh/helm/v4/pkg/chart/v2"
)

// The message of fail or required is shown when it is a literal on the
// failing line of the chart source, and withheld with a way to see it
// otherwise.
func TestChartMessageShownOnlyWhenALiteralOfTheSource(t *testing.T) {
	for _, tc := range []struct {
		name, body, values, want string
	}{
		{"required literal", `{{ required "image.tag is required" .Values.absent }}`, "", `chart message: "image.tag is required"`},
		{"fail literal on a later line", "a: 1\n{{ fail \"set the region\" }}\n", "", `chart message: "set the region"`},
		{"raw string literal", "{{ fail `region must be set` }}", "", `chart message: "region must be set"`},
		{"value message", `{{ fail .Values.message }}`, "fictional-private-value", "withheld"},
		{"printf message", `{{ fail (printf "bad %s" .Values.message) }}`, "fictional-private-value", "withheld"},
		{"value equal to a literal elsewhere", "{{/* \"fictional-private-value\" */}}\n{{ fail .Values.message }}", "fictional-private-value", "withheld"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := fixtureRequest()
			if tc.values != "" {
				req.Plan[0].Source["helm"] = map[string]any{"valuesObject": map[string]any{"message": tc.values}}
			}
			req.Repositories[testRepo+"@"+testSHA]["app/templates/deployment.yaml"] = []byte(tc.body)
			_, err := Render(req)
			var failure *domain.Failure
			if !errors.As(err, &failure) || failure.Issue.ReasonCode != domain.ReasonTemplateFailed || !strings.Contains(failure.Issue.Reason, tc.want) {
				t.Fatalf("reason %v does not contain %q", err, tc.want)
			}
			if tc.want == "withheld" && (strings.Contains(err.Error(), "fictional-private-value") || !strings.Contains(failure.Issue.Action, helmerror.ReproduceHint)) {
				t.Fatalf("value shown or no way to see the message: %+v", failure.Issue)
			}
		})
	}
}

// A subchart template is found under CHART/charts/SUBCHART, an external
// Helm's stderr is read the same way, and a chart that is not known or a
// message that is too long stays withheld.
func TestChartMessageSourceLookup(t *testing.T) {
	sub := &chart.Chart{Metadata: &chart.Metadata{Name: "sub"}, Templates: []*common.File{{Name: "templates/s.yaml", Data: []byte(`{{ required "sub value needed" .Values.x }}`)}}}
	long := strings.Repeat("x", maxChartMessage+1)
	root := &chart.Chart{Metadata: &chart.Metadata{Name: "demo"}, Templates: []*common.File{{Name: "templates/long.yaml", Data: []byte(`{{ fail "` + long + `" }}`)}}}
	root.AddDependency(sub)
	for _, tc := range []struct {
		ch   *chart.Chart
		raw  string
		want string
		ok   bool
	}{
		{root, "Error: execution error at (demo/charts/sub/templates/s.yaml:1:3): sub value needed\nUse --debug flag", "sub value needed", true},
		{nil, "execution error at (demo/charts/sub/templates/s.yaml:1:3): sub value needed", "", false},
		{root, "execution error at (other/templates/s.yaml:1:3): sub value needed", "", false},
		{root, "execution error at (demo/charts/sub/templates/s.yaml:2:3): sub value needed", "", false},
		{root, "execution error at (demo/templates/long.yaml:1:3): " + long, "", false},
	} {
		got, ok := chartMessage(tc.ch, tc.raw)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("%q: got %q %v, want %q %v", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}
