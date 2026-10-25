package localhelm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
	"helm.sh/helm/v4/pkg/chart/common"
	chart "helm.sh/helm/v4/pkg/chart/v2"
)

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testRepo = "https://github.com/example/charts"

func fixtureRequest() Request {
	source := map[string]any{"repoURL": testRepo, "path": "app", "targetRevision": testSHA, "helm": map[string]any{}}
	return Request{Application: domain.Application{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}, Spec: map[string]any{"project": "demo", "source": source, "destination": map[string]any{"name": "service-east", "namespace": "demo"}}}, Plan: []domain.SourceRevision{{Position: 1, Repository: testRepo, Revision: testSHA, Source: source}}, Settings: argocd.RenderSettings{AppLabelKey: "app.kubernetes.io/instance", TrackingMethod: "annotation", ControllerNamespace: "argocd"}, Capabilities: argocd.ClusterCapabilities{Version: "1.31.0", APIVersions: []string{"v1", "apps/v1/Deployment"}}, Limit: 1 << 20, Repositories: map[string]map[string][]byte{testRepo + "@" + testSHA: {"app/Chart.yaml": []byte("apiVersion: v2\nname: app\nversion: 0.1.0\n"), "app/values.yaml": []byte("replicas: 2\nmessage: default\n"), "app/templates/deployment.yaml": []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}
spec:
  replicas: {{ .Values.replicas }}
  template:
    metadata:
      labels: {app: api}
    spec:
      containers:
        - name: api
          image: example.invalid/api:1
          env:
            - name: MESSAGE
              value: {{ .Values.message | quote }}
`)}}}
}

func TestRenderHelmInputPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		helm     map[string]any
		replicas int
		message  string
	}{
		{"defaults", map[string]any{}, 2, "default"},
		{"ordered files", map[string]any{"valueFiles": []any{"first.yaml", "second.yaml"}}, 4, "second"},
		{"duplicate files", map[string]any{"valueFiles": []any{"first.yaml", "second.yaml", "first.yaml"}}, 4, "second"},
		{"resolved duplicate files", map[string]any{"valueFiles": []any{"first.yaml", "second.yaml", "./first.yaml"}}, 4, "second"},
		{"inline", map[string]any{"valueFiles": []any{"first.yaml"}, "values": "replicas: 5\nmessage: inline"}, 5, "inline"},
		{"object replaces inline", map[string]any{"values": "replicas: 99\nmessage: inline", "valuesObject": map[string]any{"message": "object"}}, 2, "object"},
		{"parameters", map[string]any{"valuesObject": map[string]any{"replicas": 7}, "parameters": []any{map[string]any{"name": "replicas", "value": "8"}, map[string]any{"name": "message", "value": "true", "forceString": true}}}, 8, "true"},
		{"file parameters", map[string]any{"parameters": []any{map[string]any{"name": "message", "value": "parameter"}}, "fileParameters": []any{map[string]any{"name": "message", "path": "payload.txt"}}}, 2, "a,b\n"},
		{"missing optional file", map[string]any{"valueFiles": []any{"missing.yaml"}, "ignoreMissingValueFiles": true}, 2, "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := fixtureRequest()
			req.Plan[0].Source["helm"] = tc.helm
			files := req.Repositories[testRepo+"@"+testSHA]
			files["app/first.yaml"] = []byte("replicas: 3\nmessage: first")
			files["app/second.yaml"] = []byte("replicas: 4\nmessage: second")
			files["app/payload.txt"] = []byte("a,b\n")
			out, err := Render(req)
			if err != nil {
				t.Fatal(err)
			}
			var obj map[string]any
			if len(out) != 1 || json.Unmarshal([]byte(out[0]), &obj) != nil {
				t.Fatal(out)
			}
			spec := obj["spec"].(map[string]any)
			if spec["replicas"] != float64(tc.replicas) {
				t.Fatalf("replicas = %v", spec["replicas"])
			}
			template := spec["template"].(map[string]any)
			containers := template["spec"].(map[string]any)["containers"].([]any)
			env := containers[0].(map[string]any)["env"].([]any)
			if env[0].(map[string]any)["value"] != tc.message {
				t.Fatal(env)
			}
			annotations := obj["metadata"].(map[string]any)["annotations"].(map[string]any)
			if annotations["argocd.argoproj.io/tracking-id"] != "api:apps/Deployment:demo/api" {
				t.Fatal(annotations)
			}
			again, err := Render(req)
			if err != nil || !reflect.DeepEqual(out, again) {
				t.Fatal("non-deterministic target", err)
			}
		})
	}
}

func TestRenderUncertainAndUnsafeInputs(t *testing.T) {
	for _, body := range []string{`{{ randAlphaNum 8 }}`, `{{ now }}`, `{{ tpl "{{ uuidv4 }}" . }}`, `{{ getHostByName "example.com" }}`} {
		req := fixtureRequest()
		req.Repositories[testRepo+"@"+testSHA]["app/templates/deployment.yaml"] = []byte(body)
		_, err := Render(req)
		var skipped *domain.RenderSkipped
		if !errors.As(err, &skipped) {
			t.Fatalf("%s: %v", body, err)
		}
	}
	// A fail message built from a value is withheld; only a literal of the
	// chart source may be shown.
	var err error
	for _, body := range []string{`{{ fail .Values.message }}`, `{{ fail (printf "bad value %s" .Values.message) }}`, `{{ required .Values.message .Values.absent }}`} {
		req := fixtureRequest()
		req.Plan[0].Source["helm"] = map[string]any{"valuesObject": map[string]any{"message": "fictional-private-value"}}
		req.Repositories[testRepo+"@"+testSHA]["app/templates/deployment.yaml"] = []byte(body)
		_, err = Render(req)
		if err == nil || strings.Contains(err.Error(), "fictional-private-value") || strings.Contains(err.Error(), "chart message") {
			t.Fatalf("%s: raw template failure exposed a value: %v", body, err)
		}
	}
	req := fixtureRequest()
	req.Plan[0].Source["helm"] = map[string]any{"valueFiles": []any{"../../outside.yaml"}}
	if _, err = Render(req); err == nil {
		t.Fatal("path escape accepted")
	}
	req = fixtureRequest()
	req.Limit = 1024
	req.Repositories[testRepo+"@"+testSHA]["app/values.yaml"] = []byte("message: " + strings.Repeat("x", 2000))
	if _, err = Render(req); err == nil {
		t.Fatal("output bound ignored")
	}
}

func TestSourceOverrideScopeFollowsArgoCDLookup(t *testing.T) {
	cases := []struct {
		file    string
		skipped bool
	}{
		{"app/.argocd-source.yaml", true},
		{"app/.argocd-source-api.yaml", true},
		{"app/.argocd-source-argocd_api.yaml", true},
		{"app/.argocd-source-other.yaml", false},
		{"other/.argocd-source.yaml", false},
		{"other/.argocd-source-api.yaml", false},
		{"app/templates/.argocd-source.yaml", false},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			req := fixtureRequest()
			req.Repositories[testRepo+"@"+testSHA][tc.file] = []byte("helm:\n  parameters: []\n")
			_, err := Render(req)
			var skipped *domain.RenderSkipped
			if errors.As(err, &skipped) != tc.skipped {
				t.Fatalf("skipped=%v, want %v: %v", errors.As(err, &skipped), tc.skipped, err)
			}
			if !tc.skipped && err != nil {
				t.Fatal(err)
			}
		})
	}
	// Without an Application identity every override in the source path may apply.
	req := fixtureRequest()
	req.Application.ID = domain.ApplicationID{}
	req.Repositories[testRepo+"@"+testSHA]["app/.argocd-source-other.yaml"] = []byte("helm: {}\n")
	var skipped *domain.RenderSkipped
	if _, err := Render(req); !errors.As(err, &skipped) {
		t.Fatal("anonymous request ignored a path override", err)
	}
}

func TestRenderExternalValuesAndCapabilities(t *testing.T) {
	req := fixtureRequest()
	valuesRepo := "https://github.com/example/values"
	req.Plan[0].Source["helm"] = map[string]any{"releaseName": "renamed", "valueFiles": []any{"$values/prod.yaml"}, "kubeVersion": "1.32.0", "apiVersions": []any{"example.invalid/v1/Test"}}
	req.Plan = append(req.Plan, domain.SourceRevision{Position: 2, Repository: valuesRepo, Revision: testSHA, Ref: "values", Source: map[string]any{"repoURL": valuesRepo, "ref": "values", "targetRevision": testSHA}})
	req.Repositories[valuesRepo+"@"+testSHA] = map[string][]byte{"prod.yaml": []byte("replicas: 9\nmessage: external")}
	req.Repositories[testRepo+"@"+testSHA]["app/templates/context.yaml"] = []byte(`apiVersion: v1
kind: ConfigMap
metadata: {name: context}
data:
  version: {{ .Capabilities.KubeVersion.Version | quote }}
  available: {{ .Capabilities.APIVersions.Has "example.invalid/v1/Test" | quote }}
  namespace: {{ .Release.Namespace | quote }}
`)
	out, err := Render(req)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.Join(out, "\n")
	for _, want := range []string{`"name":"renamed"`, `"replicas":9`, `"version":"v1.32.0"`, `"available":"true"`, `"namespace":"demo"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing %s: %s", want, raw)
		}
	}
}

func TestPublicDependenciesNeedNoCredentials(t *testing.T) {
	req := fixtureRequest()
	files := req.Repositories[testRepo+"@"+testSHA]
	files["app/Chart.yaml"] = []byte("apiVersion: v2\nname: app\nversion: 0.1.0\ndependencies:\n  - name: dependency\n    version: 0.1.0\n    repository: https://charts.example.invalid\n")
	c := Client{Limit: 1 << 20, Download: func(context.Context, string, int64) ([]byte, error) {
		return nil, skip("dependency needs separate authentication")
	}}
	_, err := c.prepareDependencies(context.Background(), files, nil, "app", 0)
	var skipped *domain.RenderSkipped
	if !errors.As(err, &skipped) {
		t.Fatal(err)
	}
}

func TestPublicDependencySnapshotAndLibraryValues(t *testing.T) {
	req := fixtureRequest()
	files := req.Repositories[testRepo+"@"+testSHA]
	files["app/Chart.yaml"] = []byte("apiVersion: v2\nname: app\nversion: 0.1.0\ndependencies:\n  - name: common\n    version: 0.1.0\n    repository: https://charts.example.invalid\n")
	files["app/templates/deployment.yaml"] = []byte("{{ include \"common.resource\" . }}")
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	for name, text := range map[string]string{
		"common/Chart.yaml":              "apiVersion: v2\nname: common\nversion: 0.1.0\ntype: library\n",
		"common/templates/_resource.tpl": "{{ define \"common.resource\" }}apiVersion: v1\nkind: ConfigMap\nmetadata: {name: common}\ndata: {message: '{{ .Values.message }}'}\n{{ end }}",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(text))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(text))
	}
	tw.Close()
	gz.Close()
	requests := 0
	c := Client{Limit: req.Limit, Download: func(_ context.Context, location string, _ int64) ([]byte, error) {
		requests++
		if requests > 2 {
			t.Fatal("mutable dependency was downloaded again for the opposite revision")
		}
		if strings.HasSuffix(location, "index.yaml") {
			return []byte("entries:\n  common:\n    - version: 0.1.0\n      urls: [common.tgz]\n"), nil
		}
		return data.Bytes(), nil
	}}
	for i := 0; i < 2; i++ {
		prepared, err := c.prepareDependencies(context.Background(), files, nil, "app", 0)
		if err != nil {
			t.Fatal(err)
		}
		req.Repositories[testRepo+"@"+testSHA] = prepared
		out, err := Render(req)
		if err != nil || len(out) != 1 || !strings.Contains(out[0], `"message":"default"`) {
			t.Fatal("upstream library dependency failed", out, err)
		}
	}
	if requests != 2 {
		t.Fatal(requests)
	}
	// The same package works vendored, without a network request, and its
	// expanded bytes remain subject to the configured source bound.
	vendored := make(map[string][]byte, len(files)+1)
	for name, data := range files {
		vendored[name] = data
	}
	vendored["app/charts/common-0.1.0.tgz"] = data.Bytes()
	req.Repositories[testRepo+"@"+testSHA] = vendored
	if out, err := Render(req); err != nil || len(out) != 1 {
		t.Fatal("vendored library failed", out, err)
	}
	if _, err := expandVendoredArchives(vendored, "app", 100); err == nil {
		t.Fatal("vendored expansion bypassed source limit")
	}
}

func TestUnavailableValuePathsCannotBecomeOptionalMissingFiles(t *testing.T) {
	for _, value := range []string{"https://values.example.invalid/secret.yaml", "*.yaml", "$UNAVAILABLE/file.yaml", "${ARGOCD_ENV_PRIVATE}/file.yaml"} {
		req := fixtureRequest()
		req.Plan[0].Source["helm"] = map[string]any{"ignoreMissingValueFiles": true, "valueFiles": []any{value}}
		_, err := Render(req)
		var skipped *domain.RenderSkipped
		if !errors.As(err, &skipped) {
			t.Fatal("unavailable input became an empty target", value, err)
		}
	}
}

func TestParameterCommaAndBuildEnvironment(t *testing.T) {
	for _, tc := range []struct{ value, want string }{{"a,b", "a,b"}, {`a\,b`, "a,b"}, {"${ARGOCD_APP_REVISION_SHORT}", "aaaaaaa"}, {"$$LITERAL", "$LITERAL"}} {
		req := fixtureRequest()
		req.Plan[0].Source["helm"] = map[string]any{"parameters": []any{map[string]any{"name": "message", "value": tc.value}}}
		out, err := Render(req)
		if err != nil || !strings.Contains(strings.Join(out, ""), `"value":"`+tc.want+`"`) {
			t.Fatal(tc, out, err)
		}
	}
}

func TestOverlappingParameterOrderIsExplicit(t *testing.T) {
	req := fixtureRequest()
	req.Plan[0].Source["helm"] = map[string]any{"parameters": []any{map[string]any{"name": "nested", "value": "first"}, map[string]any{"name": "nested.child", "value": "second"}}}
	_, err := Render(req)
	var skipped *domain.RenderSkipped
	if !errors.As(err, &skipped) {
		t.Fatal("backend map iteration order was silently invented", err)
	}
}

func TestRenderIsolatesUnavailableFunction(t *testing.T) {
	req := fixtureRequest()
	files := req.Repositories[testRepo+"@"+testSHA]
	files["app/templates/allowlist.yaml"] = []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "app.name" . }}-allowlist
data:
  ips: {{ concat (mustExampleIPs "demo" "region") | join "," }}
`)
	files["app/templates/_helpers.tpl"] = []byte(`{{ define "app.name" }}api{{ end }}{{ define "unused.mutating" }}{{ set .Values "message" "wrong" }}{{ end }}`)
	files["app/templates/_otherhelpers.tpl"] = []byte(`{{ define "unused.mutating" }}different{{ end }}`)
	out, err := Render(req)
	if len(out) != 1 || err == nil || !strings.Contains(err.Error(), "mustExampleIPs") || !strings.Contains(err.Error(), "allowlist.yaml:6") {
		t.Fatalf("want surviving deployment and safe template diagnostic, got %d targets, %v", len(out), err)
	}
}

func TestRenderRefusesUnsafeIsolation(t *testing.T) {
	for _, body := range []string{
		`{{ define "shared" }}{{ mustExampleIPs "demo" }}{{ end }}`,
		`{{ define "candidate" }}{{ mustExampleIPs "demo" }}{{ end }}`,
		`{{ set .Values "message" "wrong" }}{{ mustExampleIPs "demo" }}`,
		`{{ include "mutating" . }}{{ mustExampleIPs "demo" }}`,
		`{{ include .Values.message . }}{{ mustExampleIPs "demo" }}`,
		`{{ tpl .Values.message . }}{{ mustExampleIPs "demo" }}`,
		`{{ mustExampleIPs .Values }}`,
		`{{ mustExampleIPs "demo" }}{{ mustOtherFunction .Values }}`,
		`{{ .Values | mustExampleIPs }}`,
	} {
		t.Run(body, func(t *testing.T) {
			req := fixtureRequest()
			files := req.Repositories[testRepo+"@"+testSHA]
			files["app/templates/broken.yaml"] = []byte(body)
			files["app/templates/_helpers.tpl"] = []byte(`{{ define "mutating" }}{{ set .Values "message" "wrong" }}{{ end }}`)
			out, err := Render(req)
			if len(out) != 0 || err == nil || domain.IsRenderPartial(err) {
				t.Fatalf("unsafe template recovered: %v %v", out, err)
			}
		})
	}
}
func TestRenderPartialBoundsAndMultipleFailures(t *testing.T) {
	req := fixtureRequest()
	files := req.Repositories[testRepo+"@"+testSHA]
	for _, name := range []string{"first", "second"} {
		files["app/templates/"+name+".yaml"] = []byte(`{{ mustExampleIPs "fictional-private-value" }}`)
	}
	out, err := Render(req)
	if len(out) != 1 || !domain.IsRenderPartial(err) || !strings.Contains(err.Error(), "first.yaml") || !strings.Contains(err.Error(), "second.yaml") || strings.Contains(err.Error(), "fictional-private-value") {
		t.Fatalf("partial diagnostics: %v %v", out, err)
	}
	for i := 0; i < 17; i++ {
		files[fmt.Sprintf("app/templates/error-%d.yaml", i)] = []byte(`{{ mustExampleIPs "demo" }}`)
	}
	out, err = Render(req)
	if len(out) != 0 || err == nil || domain.IsRenderPartial(err) {
		t.Fatal("recovery limit ignored", err)
	}
}

// The templates parsed for one recovery serve the next only when parsing the
// remaining templates again would answer the same: a template named by its
// position, or a function name only the recovered template's actions listed,
// makes the next recovery parse again.
func TestRecoveryIndexAnswersLikeAFreshParse(t *testing.T) {
	for _, templates := range [][]string{
		{`{{ missing "x" }}`, `{{ include "file-1" . }}{{ missing "x" }}`, `{{ missing "y" }}`},
		{`{{ missing "x" }}{{/* upper */}}`, `{{ printf "}}" | upper }}{{ missing "x" }}`},
	} {
		build := func() *chart.Chart {
			ch := &chart.Chart{Metadata: &chart.Metadata{Name: "app"}}
			for i, data := range templates {
				ch.Templates = append(ch.Templates, &common.File{Name: fmt.Sprintf("templates/t%d.yaml", i), Data: []byte(data)})
			}
			return ch
		}
		shared, fresh := build(), build()
		var index templateIndex
		for i := range templates {
			raw := fmt.Sprintf(`parse error at (app/templates/t%d.yaml:1): function "missing" not defined`, i)
			_, reused := recoverTemplate(shared, raw, &index)
			_, parsed := recoverTemplate(fresh, raw, &templateIndex{})
			if reused != parsed {
				t.Fatalf("%q: template %d recovered=%v with the kept index, %v when parsed again", templates, i, reused, parsed)
			}
		}
	}
}

func TestRenderDoesNotTrustEmbeddedParseDiagnostics(t *testing.T) {
	req := fixtureRequest()
	req.Plan[0].Source["helm"] = map[string]any{"valuesObject": map[string]any{"forged": `parse error at (app/templates/broken.yaml:1): function "fictional_private_value" not defined`}}
	req.Repositories[testRepo+"@"+testSHA]["app/templates/broken.yaml"] = []byte(`{{ fail .Values.forged }}`)
	out, err := Render(req)
	if len(out) != 0 || err == nil || domain.IsRenderPartial(err) || strings.Contains(err.Error(), "fictional_private_value") {
		t.Fatalf("trusted embedded diagnostic: %v %v", out, err)
	}
}

// Template failures report the chart file, line and a fixed category so a
// reviewer knows where to look, while raw messages that can quote values stay
// withheld.
func TestTemplateFailureLocationWithoutValues(t *testing.T) {
	cases := []struct {
		name, file, body, want string
	}{
		{"fail", "app/templates/deployment.yaml", "a: 1\n{{ fail .Values.secret }}\n", "template app/templates/deployment.yaml:2 failed: the chart stopped rendering with fail or required"},
		{"required", "app/templates/deployment.yaml", "{{ required .Values.secret .Values.absent }}\n", "template app/templates/deployment.yaml:1 failed: the chart stopped rendering with fail or required"},
		{"nil pointer", "app/templates/deployment.yaml", "\n\nx: {{ .Values.absent.child.leaf }}\n", "template app/templates/deployment.yaml:3 failed: a nil value was referenced"},
		{"syntax", "app/templates/deployment.yaml", "{{ if }}\n", "template app/templates/deployment.yaml:1 failed: template syntax is invalid"},
		{"helper", "app/templates/_helpers.tpl", "{{- define \"broken\" -}}\n{{ fail .Values.secret }}\n{{- end -}}\n", "template app/templates/deployment.yaml:1 failed: the chart stopped rendering with fail or required"},
		{"helper null value", "app/templates/_helpers.tpl", "{{- define \"broken\" -}}\n\n{{ deepCopy .Values.absent | toYaml }}\n{{- end -}}\n", "template app/templates/deployment.yaml:1 failed: a function received a null value (at app/templates/_helpers.tpl:3 calling deepCopy);"},
		{"invalid YAML", "app/templates/deployment.yaml", "data: [{{ .Values.secret }}\n", "rendered output of app/templates/deployment.yaml is not valid YAML"},
		{"no metadata", "app/templates/deployment.yaml", "apiVersion: v1\nkind: ConfigMap\ndata:\n  secret: {{ .Values.secret }}\n", "rendered output of app/templates/deployment.yaml contains a document without metadata"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := fixtureRequest()
			files := req.Repositories[testRepo+"@"+testSHA]
			files["app/values.yaml"] = []byte("replicas: 2\nmessage: default\nsecret: fictional-secret-value\n")
			if strings.HasPrefix(tc.name, "helper") {
				files["app/templates/deployment.yaml"] = []byte("{{ include \"broken\" . }}\n")
			}
			files[tc.file] = []byte(tc.body)
			_, err := Render(req)
			var failure *domain.Failure
			if !errors.As(err, &failure) || failure.Issue.Code != "local_render_failed" || !strings.Contains(failure.Issue.Reason, tc.want) {
				t.Fatalf("reason does not locate the failure: %v", err)
			}
			if strings.Contains(err.Error(), "fictional-secret-value") || strings.Contains(failure.Issue.Action, "fictional-secret-value") {
				t.Fatal("template failure exposed a value", err)
			}
		})
	}
	// Unrecognized diagnostics fall back to the generic withheld message.
	if err := templateExecutionFailure(nil, "something: fictional-secret-value"); err.Error() != renderError().Error() {
		t.Fatal("unstructured diagnostic was not withheld", err)
	}
	for _, raw := range []string{"template: ../../etc/passwd:1:2: executing", "execution error at (app/values.yaml:1:2): x", "execution error at (app/templates/x.yaml:1:3): template: fictional/templates/secret.yaml:9:1"} {
		err := templateExecutionFailure(nil, raw)
		if strings.Contains(err.Error(), "etc/passwd") || strings.Contains(err.Error(), "values.yaml") || strings.Contains(err.Error(), "fictional") {
			t.Fatalf("unsafe or message-derived location reported for %q: %v", raw, err)
		}
	}
}

func TestReleaseNameFollowsHelmRules(t *testing.T) {
	for name, valid := range map[string]bool{
		"world-session-31971.115e479": true, // dotted names are valid Helm release names
		"api":                         true,
		"Upper":                       false,
		"trailing-":                   false,
		strings.Repeat("a", 54):       false,
	} {
		req := fixtureRequest()
		req.Plan[0].Source["helm"] = map[string]any{"releaseName": name}
		_, err := Render(req)
		if valid && err != nil {
			t.Errorf("%q: valid Helm release name rejected: %v", name, err)
		}
		var failure *domain.Failure
		if !valid && (!errors.As(err, &failure) || failure.Issue.Code != "local_render_failed") {
			t.Errorf("%q: invalid release name not rejected explicitly: %v", name, err)
		}
	}
}

// Like helm template and Argo CD's repo-server, lookup has no cluster and
// returns an empty result, so the fallback branch renders deterministically.
func TestRenderLookupReturnsEmptyLikeHelmTemplate(t *testing.T) {
	req := fixtureRequest()
	req.Repositories[testRepo+"@"+testSHA]["app/templates/deployment.yaml"] = []byte("{{- if lookup \"v1\" \"Secret\" \"demo\" \"token\" }}\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: live}\n{{- else }}\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: fallback}\n{{- end }}\n")
	out, err := Render(req)
	if err != nil || len(out) != 1 || !strings.Contains(out[0], "fallback") {
		t.Fatalf("lookup did not return an empty result: %v %v", out, err)
	}
}

// Expected results follow helm.sh/helm/v4 v4.2.1 (go.mod):
// pkg/release/v1/util/manifest_sorter.go (manifestFile.sort) splits
// helm.sh/hook on commas, trims and lowercases each event, maps test-success
// to the test event and drops a document with any unknown event, including
// test-failure and an empty value; pkg/cmd/template.go (isTestHook) drops
// with --skip-tests every hook whose events include test. crds/ files are
// printed without hook sorting (pkg/action/action.go renderResources).
func TestHookDocumentsFollowHelmTemplate(t *testing.T) {
	cases := []struct {
		hook                   string
		kept, keptWithoutTests bool
	}{
		{"test", true, false},
		{"Test", true, false},
		{"test, post-install", true, false},
		{" post-install , test ", true, false},
		{"test-success", true, false},
		{"test-failure", false, false},
		{"bogus", false, false},
		{"pre-install", true, true},
		{"pre-install,bogus", false, false},
		{"", false, false},
	}
	e := fakeExternal(t)
	for _, tc := range cases {
		for _, skipTests := range []bool{false, true} {
			want := tc.kept
			if skipTests {
				want = tc.keptWithoutTests
			}
			t.Run(fmt.Sprintf("%q skipTests=%t", tc.hook, skipTests), func(t *testing.T) {
				hooked := func(out []string) bool {
					if len(out) == 0 || len(out) > 2 {
						t.Fatalf("unexpected targets %v", out)
					}
					for _, doc := range out {
						if strings.Contains(doc, `"name":"hooked"`) {
							return true
						}
					}
					return false
				}
				req := fixtureRequest()
				req.Plan[0].Source["helm"] = map[string]any{"skipTests": skipTests}
				files := req.Repositories[testRepo+"@"+testSHA]
				files["app/templates/hook.yaml"] = []byte(fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: hooked\n  annotations: {helm.sh/hook: %q}\n", tc.hook))
				out, err := Render(req)
				if err != nil {
					t.Fatal(err)
				}
				if got := hooked(out); got != want {
					t.Errorf("bundled engine kept hook document = %t, want %t", got, want)
				}
				req = fixtureRequest()
				req.Plan[0].Source["helm"] = map[string]any{"skipTests": skipTests, "valuesObject": map[string]any{"externalTestCase": "hook", "hook": tc.hook}}
				out, err = e.Render(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				if got := hooked(out); got != want {
					t.Errorf("external executable output kept hook document = %t, want %t", got, want)
				}
			})
		}
	}
	// Without the annotation a document is never a hook.
	if skipHookDocument(map[string]any{"other": "test"}, true) || skipHookDocument(nil, true) {
		t.Fatal("document without helm.sh/hook dropped")
	}
	req := fixtureRequest()
	req.Plan[0].Source["helm"] = map[string]any{"skipTests": true}
	req.Repositories[testRepo+"@"+testSHA]["app/crds/crd.yaml"] = []byte("apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.example.com\n  annotations: {helm.sh/hook: bogus}\n")
	out, err := Render(req)
	if err != nil || len(out) != 2 || !strings.Contains(out[1], "widgets.example.com") {
		t.Fatalf("crds/ file sorted as a hook: %v %v", out, err)
	}
}

// A helm block key that is not an Argo CD Helm source field is an input the
// local backend would not apply, so the Application is skipped instead of
// rendered without it. Every field local Helm reads stays supported.
func TestUnknownHelmSourceFieldSkipsExplicitly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		helm   map[string]any
		reason string
	}{
		{"unknown field", map[string]any{"releaseName": "api", "valueFile": []any{"prod.yaml"}}, "Helm source fields not supported by the local backend: valueFile"},
		{"several fields", map[string]any{"skipHooks": true, "extraValues": "a: b"}, "Helm source fields not supported by the local backend: extraValues, skipHooks"},
		{"field name withheld", map[string]any{"bad key\nvalue": true}, "Helm source fields not supported by the local backend: (a field with an invalid name)"},
	} {
		req := fixtureRequest()
		req.Plan[0].Source["helm"] = tc.helm
		_, err := Render(req)
		var skipped *domain.RenderSkipped
		var failure *domain.Failure
		if !errors.As(err, &skipped) || !strings.HasSuffix(skipped.Reason, tc.reason) || !errors.As(err, &failure) || failure.Issue.Code != domain.CodeLocalContextUnavailable {
			t.Errorf("%s: not skipped explicitly: %v", tc.name, err)
		}
	}
	consumed := map[string]any{"valueFiles": []any{"values.yaml"}, "values": "message: inline", "valuesObject": map[string]any{"replicas": 3}, "parameters": []any{map[string]any{"name": "message", "value": "set"}}, "fileParameters": []any{map[string]any{"name": "message", "path": "values.yaml"}}, "releaseName": "api", "namespace": "demo", "version": "v3", "kubeVersion": "1.31.0", "apiVersions": []any{"v1"}, "ignoreMissingValueFiles": true, "skipCrds": true, "skipSchemaValidation": true, "skipTests": true, "passCredentials": true}
	if len(consumed) != len(domain.HelmSourceFields) {
		t.Fatalf("fixture covers %d of %d Helm source fields", len(consumed), len(domain.HelmSourceFields))
	}
	for field, value := range consumed {
		req := fixtureRequest()
		req.Plan[0].Source["helm"] = map[string]any{field: value}
		if _, err := Render(req); err != nil {
			t.Errorf("%s: supported Helm source field not rendered: %v", field, err)
		}
	}
}

func TestParameterNamesCannotInjectAssignments(t *testing.T) {
	e := fakeExternal(t)
	counter := filepath.Join(t.TempDir(), "renders")
	e.Environment = []string{"FICTIONAL_RENDER_COUNTER=" + counter}
	for _, helm := range []map[string]any{
		{"fileParameters": []any{map[string]any{"name": "message=/etc/hosts,replicas", "path": "payload.txt"}}},
		{"fileParameters": []any{map[string]any{"name": `message\`, "path": "payload.txt"}}},
		{"parameters": []any{map[string]any{"name": "message=injected,replicas", "value": "8"}}},
		{"parameters": []any{map[string]any{"name": "replicas,message", "value": "8", "forceString": true}}},
	} {
		for name, render := range map[string]func(Request) ([]string, error){
			"bundled":  Render,
			"external": func(req Request) ([]string, error) { return e.Render(context.Background(), req) },
		} {
			req := fixtureRequest()
			req.Plan[0].Source["helm"] = helm
			req.Repositories[testRepo+"@"+testSHA]["app/payload.txt"] = []byte("payload")
			var f *domain.Failure
			if _, err := render(req); !errors.As(err, &f) || f.Issue.Code != "local_render_failed" {
				t.Fatal(name, "parameter name injected another Helm assignment", helm, err)
			}
		}
	}
	if data, _ := os.ReadFile(counter); len(data) != 0 {
		t.Fatal("external Helm ran with an injectable parameter name")
	}
	// Escaped separators stay one literal key, as Helm parses them.
	req := fixtureRequest()
	req.Plan[0].Source["helm"] = map[string]any{"parameters": []any{map[string]any{"name": `annotations.example\.io/a\,b\=c`, "value": "x"}}}
	if _, err := Render(req); err != nil {
		t.Fatal("escaped parameter name rejected", err)
	}
}

// A source path absent at the revision is named like Argo CD's "app path does
// not exist", not reported as a generic invalid input.
func TestMissingChartPathIsNamed(t *testing.T) {
	for _, tc := range []struct {
		name, root, want string
		extra            map[string][]byte
	}{
		{"absent directory", "missing", `chart path "missing" does not exist at this revision`, nil},
		{"directory without Chart.yaml", "docs", `chart path "docs" has no Chart.yaml at this revision`, map[string][]byte{"docs/README.md": []byte("notes")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := fixtureRequest()
			for name, data := range tc.extra {
				req.Repositories[testRepo+"@"+testSHA][name] = data
			}
			req.Plan[0].Source["path"] = tc.root
			_, err := Render(req)
			var failure *domain.Failure
			if !errors.As(err, &failure) || failure.Issue.Code != "local_render_failed" || failure.Issue.Reason != tc.want || !strings.Contains(failure.Issue.Action, "app path does not exist") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// A value file absent at the revision is named by its repository-relative
// path, not reported as a generic invalid input.
func TestMissingValueFileIsNamed(t *testing.T) {
	req := fixtureRequest()
	req.Plan[0].Source["helm"] = map[string]any{"valueFiles": []any{"values.yaml", "values-new.yaml"}}
	_, err := Render(req)
	var failure *domain.Failure
	if !errors.As(err, &failure) || failure.Issue.Code != "local_render_failed" || failure.Issue.ReasonCode != "value_file_missing" || failure.Issue.Reason != `value file "app/values-new.yaml" does not exist at this revision` || !strings.Contains(failure.Issue.Action, "ignoreMissingValueFiles") {
		t.Fatalf("got %v", err)
	}
}
