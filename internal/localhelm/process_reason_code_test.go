package localhelm

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"diffaro/internal/domain"
)

// The bundled worker is the default local Helm path, so a failure that crosses
// its process boundary must keep the same issue, including the reason code
// that report titles and failure origins use, as in-process Render.
func TestWorkerFailureKeepsInProcessIssue(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		edit       func(*Request)
	}{
		{"chart path missing", "chart_path_missing", func(r *Request) { r.Plan[0].Source["path"] = "missing" }},
		{"value file missing", "value_file_missing", func(r *Request) {
			r.Plan[0].Source["helm"] = map[string]any{"valueFiles": []any{"values.yaml", "values-new.yaml"}}
		}},
		{"parameter invalid", "parameter_invalid", func(r *Request) {
			r.Plan[0].Source["helm"] = map[string]any{"parameters": []any{map[string]any{"name": "a=b", "value": "1"}}}
		}},
		{"template failed", "template_failed", func(r *Request) {
			r.Repositories[testRepo+"@"+testSHA]["app/templates/deployment.yaml"] = []byte("a: 1\n{{ fail \"fictional-private-value\" }}\n")
		}},
		{"rendered output invalid", "rendered_output_invalid", func(r *Request) {
			r.Repositories[testRepo+"@"+testSHA]["app/templates/deployment.yaml"] = []byte("kind: [\n")
		}},
		{"input invalid", "input_invalid", func(r *Request) { r.Limit = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := func() Request {
				r := fixtureRequest()
				tc.edit(&r)
				return r
			}
			_, inProcess := Render(request())
			_, worker := Process(context.Background(), request())
			var want, got *domain.Failure
			if !errors.As(inProcess, &want) || !errors.As(worker, &got) {
				t.Fatalf("not structured failures: in-process %v, worker %v", inProcess, worker)
			}
			if want.Issue.ReasonCode != tc.want {
				t.Fatalf("fixture produced reasonCode %q, want %q", want.Issue.ReasonCode, tc.want)
			}
			if !reflect.DeepEqual(got.Issue, want.Issue) {
				t.Fatalf("worker issue differs from in-process Render:\n got %+v\nwant %+v", got.Issue, want.Issue)
			}
		})
	}
}
