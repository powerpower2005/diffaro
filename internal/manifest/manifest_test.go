package manifest

import (
	"diffaro/internal/domain"
	"diffaro/internal/security"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const deployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 2
`

func parsed(t *testing.T, raw string) map[string]Resource {
	t.Helper()
	r, err := Parse([]string{raw}, "test:cluster", "demo", Limits{Bytes: 1 << 20, Documents: 100}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSemanticDiff(t *testing.T) {
	b := parsed(t, deployment)
	h := parsed(t, strings.Replace(deployment, "replicas: 2", "replicas: 3", 1))
	diff, complete := Compare(b, h, security.Redactor{})
	if !complete || len(diff) != 1 || diff[0].State != "changed" || len(diff[0].Changes) != 1 || diff[0].Changes[0].Path != "/spec/replicas" {
		t.Fatalf("unexpected diff: %+v", diff)
	}
	if diff[0].ID.Namespace != "demo" {
		t.Fatal("destination namespace lost")
	}
	h = parsed(t, strings.Replace(deployment, "  name: api", "  name: api\n  resourceVersion: '999'", 1))
	diff, _ = Compare(b, h, security.Redactor{})
	if diff[0].State != "unchanged" {
		t.Fatal("runtime metadata was compared")
	}
}

func TestAddedRemovedAndOrder(t *testing.T) {
	b := parsed(t, deployment)
	h := parsed(t, strings.Replace(deployment, "name: api", "name: new-api", 1))
	diff, _ := Compare(b, h, security.Redactor{})
	if len(diff) != 2 || diff[0].State != "removed" || diff[1].State != "added" {
		t.Fatalf("bad identity diff: %+v", diff)
	}
	first, _ := json.Marshal(diff)
	for i := 0; i < 10; i++ {
		again, _ := Compare(b, h, security.Redactor{})
		next, _ := json.Marshal(again)
		if string(next) != string(first) {
			t.Fatal("unstable ordering")
		}
	}
}

// TestSecretChangesAreReportedWithoutValues verifies that a readable
// Secret payload is compared key by key and only the fact of a change is
// reported; a payload Argo CD masked stays unknown.
func TestSecretChangesAreReportedWithoutValues(t *testing.T) {
	secret := func(value string) map[string]Resource {
		return parsed(t, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: credentials\n  annotations:\n    kubectl.kubernetes.io/last-applied-configuration: fictional-sensitive-inline\ndata:\n  key: "+value+"\n  same: fictional-same\n")
	}
	diff, complete := Compare(secret("fictional-a"), secret("fictional-a"), security.Redactor{})
	if !complete || diff[0].State != "unchanged" {
		t.Fatalf("equal readable payloads not compared: %+v", diff[0])
	}
	diff, complete = Compare(secret("fictional-a"), secret("fictional-b"), security.Redactor{})
	encoded, _ := json.Marshal(diff)
	if !complete || diff[0].State != "changed" || len(diff[0].Changes) != 1 || diff[0].Changes[0].Path != "/data/key" || !diff[0].Changes[0].Redacted || strings.Contains(string(encoded), "fictional-") {
		t.Fatalf("changed key not reported, or a value leaked: %s", encoded)
	}
	diff, complete = Compare(secret("++++++++"), secret("++++++++"), security.Redactor{})
	if complete || diff[0].State != "unknown" || diff[0].Comparison != "incomplete" || diff[0].IncompleteReason != domain.IncompleteMaskedSecret {
		t.Fatalf("payload masked by Argo CD claimed comparable, or its cause is unnamed: %+v", diff[0])
	}
	diff, _ = Compare(map[string]Resource{}, secret("fictional-a"), security.Redactor{})
	encoded, _ = json.Marshal(diff)
	if diff[0].State != "added" || strings.Contains(string(encoded), "fictional-") {
		t.Fatal("Secret creation not observable, or its payload leaked")
	}
}

func TestSensitiveChangesStillDetected(t *testing.T) {
	b := parsed(t, `apiVersion: v1
kind: ConfigMap
metadata: {name: settings}
data: {password: fictional-old, public: old}
`)
	h := parsed(t, `apiVersion: v1
kind: ConfigMap
metadata: {name: settings}
data: {password: fictional-new, public: new}
`)
	diff, complete := Compare(b, h, security.Redactor{Paths: []string{"/data/public"}})
	encoded, _ := json.Marshal(diff)
	if !complete || diff[0].State != "changed" || strings.Contains(string(encoded), "fictional-") || strings.Contains(string(encoded), `"new"`) {
		t.Fatal("sensitive comparison/redaction contract broken")
	}
}

func TestClusterScopedNamespaceIsIgnoredLikeArgoCD(t *testing.T) {
	const role = "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata: {name: reader, namespace: reflector}\nrules: []\n"
	b := parsed(t, role)
	h := parsed(t, strings.Replace(role, ", namespace: reflector", "", 1))
	for _, side := range []map[string]Resource{b, h} {
		if len(side) != 1 {
			t.Fatalf("resources: %+v", side)
		}
		for _, r := range side {
			meta := r.Object["metadata"].(map[string]any)
			if r.ID.Namespace != "" || meta["namespace"] != nil {
				t.Fatalf("cluster-scoped namespace kept: %+v %+v", r.ID, meta)
			}
		}
	}
	diff, complete := Compare(b, h, security.Redactor{})
	if !complete || len(diff) != 1 || diff[0].State != "unchanged" {
		t.Fatalf("ignored namespace changed the comparison: %+v", diff)
	}
	changed := parsed(t, strings.Replace(role, "rules: []", "rules: [{apiGroups: [''], resources: [secrets], verbs: [get]}]", 1))
	if diff, _ := Compare(b, changed, security.Redactor{}); len(diff) != 1 || diff[0].State != "changed" {
		t.Fatalf("rule change hidden: %+v", diff)
	}
}

func TestRejectUnsafeOrAmbiguousYAML(t *testing.T) {
	for _, tt := range []struct{ name, raw string }{
		{"alias", "apiVersion: v1\nkind: ConfigMap\nmetadata: &x {name: x}\ndata: *x\n"},
		{"duplicate key", deployment + "spec: {replicas: 5}\n"},
		{"duplicate resource", deployment + "---\n" + deployment},
		{"generate name", "apiVersion: v1\nkind: Pod\nmetadata: {generateName: preview-}\n"},
		{"unknown scope", "apiVersion: demo.example.com/v1\nkind: Widget\nmetadata: {name: sample}\n"},
		{"cluster-scoped identity conflict after namespace removal", "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata: {name: reader, namespace: a}\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata: {name: reader, namespace: b}\n"},
		{"nonfinite value", deployment + "  invalidNumber: .nan\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]string{tt.raw}, "c", "n", Limits{Bytes: 1 << 20, Documents: 100}, nil); err == nil {
				t.Fatal("unsafe input accepted")
			}
		})
	}
}

func TestRenderedApplicationInputsAreRedacted(t *testing.T) {
	app := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: child, namespace: argocd}
spec:
  source:
    helm:
      values: "connection: fictional-inline-sensitive"
      valuesObject: {connection: fictional-object-sensitive}
      parameters: [{name: arbitrary, value: fictional-parameter-sensitive}]
`
	base := parsed(t, app)
	head := parsed(t, strings.ReplaceAll(app, "fictional-", "updated-fictional-"))
	for _, pair := range [][2]map[string]Resource{{{}, head}, {base, head}} {
		diff, complete := Compare(pair[0], pair[1], security.Redactor{})
		b, _ := json.Marshal(diff)
		if !complete || diff[0].State == "unchanged" || strings.Contains(string(b), "fictional-") {
			t.Fatal("rendered Application input leaked or change was hidden")
		}
	}
}

func TestNumericPrecisionAndListOrder(t *testing.T) {
	base := deployment + "  revisionNumber: 9007199254740992\n  args: [a, b]\n"
	head := strings.Replace(base, "9007199254740992", "9007199254740993", 1)
	head = strings.Replace(head, "[a, b]", "[b, a]", 1)
	diff, _ := Compare(parsed(t, base), parsed(t, head), security.Redactor{})
	if len(diff[0].Changes) != 2 {
		t.Fatal("large integer or list order difference lost")
	}
}

func TestLimits(t *testing.T) {
	if _, err := Parse([]string{strings.Repeat("---\n", 10)}, "c", "n", Limits{Bytes: 1 << 20, Documents: 2}, nil); err == nil {
		t.Fatal("empty document count limit ignored")
	}
	if _, err := Parse([]string{deployment}, "c", "n", Limits{Bytes: 10, Documents: 100}, nil); err == nil {
		t.Fatal("byte limit ignored")
	}
	if _, err := Parse([]string{deployment + "---\n" + strings.Replace(deployment, "name: api", "name: api2", 1)}, "c", "n", Limits{Bytes: 1 << 20, Documents: 1}, nil); err == nil {
		t.Fatal("document limit ignored")
	}
}

func TestLimitDiagnosticsDistinguishBytesDocumentsAndListResources(t *testing.T) {
	for _, tc := range []struct {
		name, raw, setting, observed string
		limits                       Limits
	}{
		{"bytes", deployment, "render.maxResponseBytes", "bytes; limit 10 bytes", Limits{Bytes: 10, Documents: 100}},
		{"empty documents", strings.Repeat("---\n", 5), "render.maxDocuments", "observed at least 3 documents; limit 2 documents", Limits{Bytes: 1024, Documents: 2}},
		{"List resources", `{"apiVersion":"v1","kind":"List","items":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"}},{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"b"}},{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"}}]}`, "render.maxDocuments", "observed at least 3 resources; limit 2 resources", Limits{Bytes: 1024, Documents: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]string{tc.raw}, "test", "demo", tc.limits, nil)
			var failure *domain.Failure
			if !errors.As(err, &failure) || failure.Issue.Code != "limit_exceeded" || !strings.Contains(failure.Issue.Reason, tc.setting) || !strings.Contains(failure.Issue.Reason, tc.observed) || !strings.Contains(failure.Issue.Action, tc.setting) {
				t.Fatal("parse limit lacks a measured recovery action", err)
			}
		})
	}
}

func TestPartialComparisonRequiresCompleteOppositeForAbsence(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		baseComplete, headComplete bool
		removed, added             string
	}{
		{"both complete", true, true, "removed", "added"},
		{"base incomplete", false, true, "removed", "unknown"},
		{"head incomplete", true, false, "unknown", "added"},
		{"both incomplete", false, false, "unknown", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := parsed(t, deployment)
			head := parsed(t, strings.Replace(deployment, "name: api", "name: new-api", 1))
			out, secretComplete := ComparePartial(base, head, security.Redactor{}, tc.baseComplete, tc.headComplete)
			if !secretComplete || len(out) != 2 || string(out[0].State) != tc.removed || string(out[1].State) != tc.added {
				t.Fatalf("bad partial comparison: %+v", out)
			}
			for _, res := range out {
				if res.State == "unknown" && (res.Comparison != "incomplete" || len(res.Changes) != 0 || res.IncompleteReason != domain.IncompletePartialTarget) {
					t.Fatal("unknown resource claims a change or does not name the partial side", res)
				}
				if res.State != "unknown" && res.IncompleteReason != "" {
					t.Fatal("a compared resource names an incomplete cause", res)
				}
			}
		})
	}
}

// Comparing a Secret records its payload, decoded data included, so value
// file changes can mask the same values; other fields are not
// recorded.
func TestSecretComparisonRecordsPayloadValues(t *testing.T) {
	masked := &security.MaskedValues{}
	r := security.Redactor{Masked: masked}
	secret := func(token string) map[string]Resource {
		return parsed(t, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: credentials\n  labels:\n    app: fictional-label-value\ndata:\n  token: "+base64.StdEncoding.EncodeToString([]byte(token))+"\nstringData:\n  key: fictional-string-data\n")
	}
	Compare(secret("fictional-old-token"), secret("fictional-new-token"), r)
	for _, value := range []string{"fictional-old-token", "fictional-new-token", "fictional-string-data"} {
		if !masked.Hides(value) {
			t.Errorf("Secret payload %q not recorded", value)
		}
	}
	if masked.Hides("fictional-label-value") {
		t.Error("a non-payload field was recorded")
	}
	// An added Secret is recorded too.
	added := &security.MaskedValues{}
	Compare(map[string]Resource{}, secret("fictional-added-token"), security.Redactor{Masked: added})
	if !added.Hides("fictional-added-token") {
		t.Error("added Secret payload not recorded")
	}
}
