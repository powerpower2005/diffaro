package localhelm

import (
	"context"
	"strings"
	"testing"
)

// The probe renders a freshly loaded copy without changing the chart passed
// to the external executable. Bundled dependency processing
// drops a condition-disabled subchart; the external executable must still
// receive it.
func TestExternalProbeDoesNotChangeChartPassedToExecutable(t *testing.T) {
	e := fakeExternal(t)
	req := fixtureRequest()
	files := req.Repositories[testRepo+"@"+testSHA]
	files["app/Chart.yaml"] = []byte("apiVersion: v2\nname: app\nversion: 0.1.0\ndependencies:\n  - name: db\n    version: 0.1.0\n    condition: db.enabled\n")
	files["app/values.yaml"] = []byte("replicas: 2\nmessage: default\ndb: {enabled: false}\nexternalTestCase: report-subchart\n")
	files["app/charts/db/Chart.yaml"] = []byte("apiVersion: v2\nname: db\nversion: 0.1.0\n")
	files["app/charts/db/templates/config.yaml"] = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: db}\n")
	// An inactive time reference makes the external render run the probe.
	files["app/templates/config.yaml"] = []byte(`{{ if false }}{{ now }}{{ end }}`)
	got, err := e.Render(context.Background(), req)
	if err != nil || len(got) != 1 {
		t.Fatal("probe-cleared chart did not render externally", err, got)
	}
	if !strings.Contains(got[0], "present") {
		t.Fatal("the probe changed the chart passed to the external executable: condition-disabled subchart dropped", got[0])
	}
}
