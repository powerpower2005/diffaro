package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

func TestExternalHelmFirstUseAndNewChild(t *testing.T) {
	binary := os.Getenv("DIFFARO_TEST_HELM_BINARY")
	if binary == "" {
		t.Skip("set DIFFARO_TEST_HELM_BINARY for external Helm first-use integration")
	}
	testHelmFirstUseAndNewChild(t, binary)
}

func TestBundledHelmFirstUseAndNewChild(t *testing.T) {
	testHelmFirstUseAndNewChild(t, "")
}

func TestHelmPartialTargetsCLI(t *testing.T) {
	t.Run("bundled", func(t *testing.T) { testHelmPartialTargets(t, "") })
	if binary := os.Getenv("DIFFARO_TEST_HELM_BINARY"); binary != "" {
		t.Run("external", func(t *testing.T) { testHelmPartialTargets(t, binary) })
	}
}

// helmFirstUse is a first use of local Helm against a fake GitHub that serves
// a parent chart whose head revision adds the uninstalled child Application
// new-api, and a read-only fake Argo CD. With partial, the parent chart also
// has a template that fails at both revisions next to a ConfigMap that
// changes. Commands run in process or as the installed binary (runDiffaro)
// with an empty PATH.
type helmFirstUse struct {
	t                  *testing.T
	dir, config        string
	gitURL, argoURL    string
	markdown, jsonFile string
	out, diagnostic    bytes.Buffer
}

// newHelmFirstUse creates the profile with `init --local-children`, selecting
// the bundled engine or the external Helm executable binary, and checks
// that the selection was saved. Later commands run without the API token
// environment.
func newHelmFirstUse(t *testing.T, binary string, partial bool) *helmFirstUse {
	t.Helper()
	// filesAt is the repository at revision; only the head changes the
	// parent's values.
	filesAt := func(revision string) map[string]string {
		changed := revision == headSHA
		files := map[string]string{"parent/Chart.yaml": "apiVersion: v2\nname: parent\nversion: 0.1.0\n", "parent/values.yaml": fmt.Sprintf("changed: %t\n", changed), "parent/templates/apps.yaml": `{{- if .Values.changed }}
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: new-api, namespace: argocd}
spec:
  project: demo
  source:
    repoURL: https://github.com/example/charts.git
    path: child
    targetRevision: main
    helm: {valuesObject: {replicas: 3}}
  destination: {name: test-cluster, namespace: demo}
{{- end }}
`, "child/Chart.yaml": "apiVersion: v2\nname: child\nversion: 0.1.0\n", "child/templates/deployment.yaml": `apiVersion: apps/v1
kind: Deployment
metadata: {name: {{ .Release.Name }}}
spec:
  replicas: {{ .Values.replicas }}
  selector: {matchLabels: {app: {{ .Release.Name }}}}
  template:
    metadata: {labels: {app: {{ .Release.Name }}}}
    spec:
      containers: [{name: api, image: example.invalid/api:1}]
`, "child/templates/service.yaml": `apiVersion: v1
kind: Service
metadata: {name: {{ .Release.Name }}}
spec:
  selector: {app: {{ .Release.Name }}}
  ports: [{port: 80, targetPort: 8080}]
`}
		if partial {
			files["parent/templates/allowlist.yaml"] = `{{ mustExampleIPs "fictional-private-value" }}`
			files["parent/templates/settings.yaml"] = `apiVersion: v1
kind: ConfigMap
metadata: {name: settings}
data: {changed: {{ .Values.changed | quote }}}
`
		}
		return files
	}
	git := newGitHubFake(t, githubFake{token: "fictional-github-token", routes: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/repos/example/charts/pulls/13" || r.URL.Path == "/repos/example/charts/pulls/12" {
			fmt.Fprintf(w, `{"state":"open","changed_files":1,"base":{"sha":%q,"repo":{"full_name":"example/charts"}},"head":{"sha":%q,"repo":{"full_name":"example/charts"}}}`, baseSHA, headSHA)
			return true
		}
		// init --pr writes changed-path selection, which reads the
		// PR file list and repository files; the PR changes the parent's values.
		if r.URL.Path == "/repos/example/charts/pulls/13/files" || r.URL.Path == "/repos/example/charts/pulls/12/files" {
			fmt.Fprint(w, `[{"filename":"parent/values.yaml","status":"modified"}]`)
			return true
		}
		if name, ok := strings.CutPrefix(r.URL.Path, "/repos/example/charts/contents/"); ok {
			data, found := filesAt(r.URL.Query().Get("ref"))[name]
			if !found {
				return false
			}
			fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(data)))
			return true
		}
		if strings.Contains(r.URL.Path, "/tarball/") {
			files := filesAt(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			gz := gzip.NewWriter(w)
			tw := tar.NewWriter(gz)
			for name, data := range files {
				_ = tw.WriteHeader(&tar.Header{Name: "archive/" + name, Mode: 0600, Size: int64(len(data))})
				_, _ = tw.Write([]byte(data))
			}
			_ = tw.Close()
			_ = gz.Close()
			return true
		}
		return false
	}})
	app := map[string]any{"metadata": map[string]any{"name": "parent", "namespace": "argocd"}, "spec": map[string]any{"project": "demo", "source": map[string]any{"repoURL": fakeRepo, "path": "parent", "targetRevision": "main", "helm": map[string]any{}}, "destination": map[string]any{"name": "in-cluster", "namespace": "argocd"}}}
	argo := newArgoCDFake(t, argocdFake{token: "fictional-argo-0", version: "v3.5.1", project: `{"metadata":{"name":"demo"},"spec":{"sourceRepos":["*"],"destinations":[{"name":"*","namespace":"*"}]}}`, apps: func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/applications":
			json.NewEncoder(w).Encode(map[string]any{"items": []any{app}})
		case "/api/v1/applications/parent":
			json.NewEncoder(w).Encode(app)
		case "/api/v1/settings":
			fmt.Fprint(w, `{"appLabelKey":"app.kubernetes.io/instance","trackingMethod":"annotation","controllerNamespace":"argocd"}`)
		case "/api/v1/clusters/test-cluster":
			fmt.Fprint(w, `{"name":"test-cluster","server":"https://cluster.example.invalid","info":{"serverVersion":"1.31.0","apiVersions":["v1","apps/v1/Deployment"],"connectionState":{"status":"Successful"}}}`)
		case "/api/v1/clusters/in-cluster":
			fmt.Fprint(w, `{"name":"in-cluster","server":"https://kubernetes.default.svc","info":{"serverVersion":"1.31.0","apiVersions":["v1","apps/v1/Deployment","argoproj.io/v1alpha1/Application"],"connectionState":{"status":"Successful"}}}`)
		default:
			t.Errorf("unexpected installed-child lookup, manifest call or backend request: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}})
	t.Setenv("DIFFARO_GITHUB_TOKEN", "fictional-github-token")
	t.Setenv("DIFFARO_ARGOCD_TOKEN", "fictional-argo-0")
	t.Setenv("KUBECONFIG", "must-not-be-read")
	t.Setenv("PATH", "")
	dir := t.TempDir()
	catalog := filepath.Join(dir, "connections.yaml")
	config := filepath.Join(dir, "profile with spaces.yaml")
	_ = os.WriteFile(catalog, []byte("instances:\n  - id: demo\n    endpoint: "+argo.URL+"\n    tokenEnv: DIFFARO_ARGOCD_TOKEN\n"), 0600)
	f := &helmFirstUse{t: t, dir: dir, config: config, gitURL: git.URL, argoURL: argo.URL, markdown: filepath.Join(dir, "preview.md"), jsonFile: filepath.Join(dir, "preview.json")}
	args := []string{"init", "--connections", catalog, "--local-children", "--pr", "https://github.com/example/charts/pull/12", "--git-api", git.URL, "--config", config}
	engineName := "bundled local Helm"
	if binary != "" {
		args = append(args, "--helm-binary", binary)
		engineName = "external Helm"
	} else {
		args = append(args, "--local-helm")
	}
	if code := f.run(args...); code != 0 || !strings.Contains(f.out.String(), engineName) {
		t.Fatal(code, f.out.String(), f.diagnostic.String())
	}
	c, err := profile.Load(config)
	if err != nil || !c.Instances[0].LocalHelm || c.Instances[0].HelmBinary != binary {
		t.Fatal("external engine selection not persisted", err)
	}
	t.Setenv("DIFFARO_GITHUB_TOKEN", "")
	t.Setenv("DIFFARO_ARGOCD_TOKEN", "")
	return f
}

// run executes one command with fresh output buffers.
func (f *helmFirstUse) run(args ...string) int {
	f.out.Reset()
	f.diagnostic.Reset()
	return runDiffaro(f.t, args, &f.out, &f.diagnostic)
}

// preview runs the rendered doctor preflight and the PR 12 diff that saves
// Markdown and JSON, expecting exit for both, and returns the JSON report.
func (f *helmFirstUse) preview(exit int) []byte {
	f.t.Helper()
	if code := f.run("doctor", "--config", f.config, "--render", "--pr", "12", "--format", "json", "--quiet"); code != exit {
		f.t.Fatal("first render preflight failed", code, f.out.String(), f.diagnostic.String())
	}
	if code := f.run("diff", "--config", f.config, "--pr", "12", "--format", "markdown", "--output", f.markdown, "--json-output", f.jsonFile, "--quiet"); code != exit {
		f.t.Fatal("PR preview failed", code, f.out.String(), f.diagnostic.String())
	}
	data, err := os.ReadFile(f.jsonFile)
	if err != nil {
		f.t.Fatal(err)
	}
	return data
}

// saveEvidence copies outputs to the directory named by the environment
// variable env, when it is set.
func saveEvidence(t *testing.T, env string, files map[string][]byte) {
	t.Helper()
	dir := os.Getenv(env)
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0644); err != nil {
			t.Fatal(err)
		}
	}
}

// testHelmFirstUseAndNewChild checks that the first PR preview renders the
// uninstalled child with its resources, repeats deterministically in every
// format and works for another PR without regenerating the profile.
func testHelmFirstUseAndNewChild(t *testing.T, binary string) *helmFirstUse {
	t.Helper()
	f := newHelmFirstUse(t, binary, false)
	data := f.preview(1)
	var report domain.Report
	if json.Unmarshal(data, &report) != nil || report.Status != "success" || len(report.Applications) != 2 || report.Revisions.Base != ancestorSHA || report.Revisions.Head != headSHA {
		t.Fatal("target graph or resolved PR comparison lost", report.Issues)
	}
	newChild, newService := false, false
	for _, a := range report.Applications {
		if a.ID.Name == "new-api" && a.State == "added" && a.Execution == "complete" {
			for _, r := range a.Resources {
				newChild = newChild || (r.ID.Kind == "Deployment" && r.State == "added")
				newService = newService || (r.ID.Kind == "Service" && r.State == "added")
			}
		}
	}
	if !newChild || !newService || len(report.RenderBackends) != 1 || (binary != "" && len(report.RenderBackends[0].HelmSHA256) != 64) {
		t.Fatal("uninstalled child or engine provenance absent")
	}
	md, _ := os.ReadFile(f.markdown)
	assertNoSecrets(t, "JSON report", string(data), "fictional-")
	if !bytes.Contains(md, []byte(report.RenderBackends[0].HelmSHA256)) {
		t.Fatal("missing Helm identity")
	}
	if code := f.run("diff", "--config", f.config, "--pr", "12", "--format", "json", "--quiet"); code != 1 || !bytes.Equal(data, f.out.Bytes()) {
		t.Fatal("CI repeat result is not deterministic", code, f.diagnostic.String())
	}
	terminalJSON := filepath.Join(f.dir, "terminal.json")
	code := f.run("diff", "--config", f.config, "--pr", "12", "--format", "terminal", "--json-output", terminalJSON, "--quiet")
	terminalData, readErr := os.ReadFile(terminalJSON)
	out := f.out.String()
	if code != 1 || readErr != nil || !bytes.Equal(data, terminalData) || !strings.Contains(out, "[+] App demo/argocd/new-api") || !strings.Contains(out, "Deployment/new-api") || !strings.Contains(out, "Service/new-api") || !strings.Contains(out, "/spec/replicas: 3") {
		t.Fatal("terminal first-use lost the new child or changed the comparison", code, readErr, out, f.diagnostic.String())
	}
	// The profile contains repository selection, not a frozen PR number.
	beforeProfile, _ := os.ReadFile(f.config)
	code = f.run("diff", "--config", f.config, "--pr", "13", "--recursive", "--format", "terminal", "--quiet")
	afterProfile, _ := os.ReadFile(f.config)
	if code != 1 || !bytes.Equal(beforeProfile, afterProfile) || !strings.Contains(f.out.String(), "Deployment/new-api") {
		t.Fatal("another PR requires regenerating the profile", code, f.diagnostic.String())
	}
	saveEvidence(t, "DIFFARO_EXTERNAL_EVIDENCE_DIR", map[string][]byte{"preview.json": data, "preview.md": md, "preview.txt": f.out.Bytes()})
	return f
}

// testHelmPartialTargets checks that a template failing at both revisions
// keeps the confirmed change and the unknown child, reports the failure in
// the terminal without its private input, repeats deterministically and is
// located by --trace.
func testHelmPartialTargets(t *testing.T, binary string) {
	t.Helper()
	f := newHelmFirstUse(t, binary, true)
	data := f.preview(3)
	var report domain.Report
	if json.Unmarshal(data, &report) != nil || report.Status != "partial_failure" || report.ExitCode() != 3 || !report.ChangesFound || len(report.Applications) != 1 || report.Graph.BaseComplete || report.Graph.HeadComplete {
		t.Fatalf("partial binary report: %+v", report)
	}
	a := report.Applications[0]
	if a.Execution != "failed" || a.BaseTarget.Execution != "failed" || a.HeadTarget.Execution != "failed" || len(a.Issues) != 2 || len(a.Resources) != 2 {
		t.Fatal("partial binary lost targets/errors", a)
	}
	for _, r := range a.Resources {
		if r.ID.Name == "new-api" && r.State != "unknown" {
			t.Fatal("incomplete base became an addition", r)
		}
		if r.ID.Name == "settings" && r.State != "changed" {
			t.Fatal("confirmed change lost", r)
		}
	}
	code := f.run("diff", "--config", f.config, "--pr", "12", "--format", "terminal", "--quiet")
	out := f.out.String()
	if code != 3 || !strings.Contains(out, "ConfigMap/settings") || !strings.Contains(out, "allowlist.yaml:1") || !strings.Contains(out, "mustExampleIPs") {
		t.Fatal("partial terminal diagnostics", code, out, f.diagnostic.String())
	}
	assertNoSecrets(t, "partial terminal diagnostics", out, "fictional-private-value")
	terminal := append([]byte(nil), f.out.Bytes()...)
	if code = f.run("diff", "--config", f.config, "--pr", "12", "--format", "json", "--quiet"); code != 3 || !bytes.Equal(data, f.out.Bytes()) {
		t.Fatal("partial results are not deterministic", code, f.diagnostic.String())
	}
	f.assertPartialTrace(data)
	saveEvidence(t, "DIFFARO_PARTIAL_EVIDENCE_DIR", map[string][]byte{"preview.json": data, "preview.txt": terminal})
}

func TestExternalHelmInitAndProfileValidationOffline(t *testing.T) {
	dir := t.TempDir()
	catalog := filepath.Join(dir, "connections.yaml")
	config := filepath.Join(dir, "profile.yaml")
	_ = os.WriteFile(catalog, []byte("instances:\n  - id: demo\n    endpoint: https://argo.example.invalid\n    tokenEnv: TEST_ARGO_TOKEN\n"), 0600)
	var out, diagnostic bytes.Buffer
	code := Run(context.Background(), []string{"init", "--connections", catalog, "--local-children", "--pr", "https://github.com/example/charts/pull/12", "--helm-binary", filepath.Join(dir, "missing helm"), "--config", config}, &out, &diagnostic)
	if code != 0 {
		t.Fatal("offline init must save the selection without execution", code, diagnostic.String())
	}
	if code = Run(context.Background(), []string{"validate", "--config", config}, &out, &diagnostic); code != 0 {
		t.Fatal("offline profile validation attempted execution", code)
	}
}

func TestExternalHelmSetupFailurePreservesOtherInstances(t *testing.T) {
	g := gitServer(t)
	first, failedRenders := argoServer(t, "fictional-argo-0", backendOptions{})
	second, _ := argoServer(t, "fictional-argo-1", backendOptions{workload: true})
	config := configFile(t, g.URL, first.URL, second.URL)
	editProfile(t, config, "    tokenEnv: DIFFARO_TEST_ARGO_0", "    tokenEnv: DIFFARO_TEST_ARGO_0\n    localHelm: true\n    helmBinary: missing-executable")
	var out, diagnostic bytes.Buffer
	code := Run(context.Background(), []string{"diff", "--config", config, "--base", "main", "--head", "feature", "--format", "json", "--quiet"}, &out, &diagnostic)
	var report domain.Report
	if code != 3 || json.Unmarshal(out.Bytes(), &report) != nil || report.Status != "partial_failure" || len(report.Applications) != 2 || len(*failedRenders) != 0 {
		t.Fatal("external setup failed globally or fell back", code, diagnostic.String())
	}
	if report.Applications[0].Execution != "failed" || report.Applications[1].Execution != "complete" {
		t.Fatal("independent results hidden", report.Issues)
	}
	if !strings.Contains(out.String(), "helm_execution_failed") {
		t.Fatal("failure context lost")
	}
	assertNoSecrets(t, "diff output", out.String()+diagnostic.String(), "fictional-")
}

func TestExternalHelmEnvironmentConfiguration(t *testing.T) {
	g := gitServer(t)
	first, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	second, _ := argoServer(t, "fictional-argo-1", backendOptions{workload: true})
	environment := "\n    helmEnvironment:\n      - name: METADATA_HOST\n        value: https://metadata.example.invalid\n      - name: METADATA_TOKEN\n        valueFromEnv: TEST_METADATA_TOKEN"
	write := func(t *testing.T, instance string) string {
		t.Helper()
		config := configFile(t, g.URL, first.URL, second.URL)
		editProfile(t, config, "    tokenEnv: DIFFARO_TEST_ARGO_0", "    tokenEnv: DIFFARO_TEST_ARGO_0\n    localHelm: true"+instance+environment)
		return config
	}
	run := func(config string, extra ...string) (int, string, string) {
		var out, diagnostic bytes.Buffer
		code := Run(context.Background(), append([]string{"diff", "--config", config, "--base", "main", "--head", "feature", "--format", "json", "--quiet"}, extra...), &out, &diagnostic)
		return code, out.String(), diagnostic.String()
	}
	t.Run("requires external executable", func(t *testing.T) {
		t.Setenv("TEST_METADATA_TOKEN", "fictional-metadata-token")
		if code, _, diagnostic := run(write(t, "")); code != 4 || !strings.Contains(diagnostic, "helmEnvironment requires an external Helm executable") {
			t.Fatal("bundled Helm silently ignored configured environment", code, diagnostic)
		}
	})
	t.Run("unset secret source is invalid configuration", func(t *testing.T) {
		code, out, diagnostic := run(write(t, "\n    helmBinary: missing-executable"))
		if code != 4 || !strings.Contains(diagnostic, "TEST_METADATA_TOKEN") {
			t.Fatal("missing environment value not explicit", code, diagnostic)
		}
		assertNoSecrets(t, "diff output", out+diagnostic, "fictional-metadata")
	})
	t.Run("limitation and redaction preserve partial results", func(t *testing.T) {
		t.Setenv("TEST_METADATA_TOKEN", "fictional-metadata-token")
		code, out, diagnostic := run(write(t, "\n    helmBinary: missing-executable"))
		var report domain.Report
		if code != 3 || json.Unmarshal([]byte(out), &report) != nil || report.Status != "partial_failure" {
			t.Fatal("environment setup changed failure semantics", code, diagnostic)
		}
		limited := false
		for _, limitation := range report.Limitations {
			limited = limited || strings.Contains(limitation, "operator-configured environment variables")
		}
		if !limited {
			t.Fatal("external input limitation missing", report.Limitations)
		}
		assertNoSecrets(t, "diff output", out+diagnostic, "fictional-metadata-token")
	})
}

// assertPartialTrace checks that --trace names the failing step without
// changing the report or exposing credentials and request URLs.
func (f *helmFirstUse) assertPartialTrace(report []byte) {
	t := f.t
	t.Helper()
	traceFile := filepath.Join(f.dir, "trace.json")
	code := f.run("diff", "--config", f.config, "--pr", "12", "--format", "json", "--trace", traceFile, "--quiet")
	if code != 3 || !bytes.Equal(report, f.out.Bytes()) {
		t.Fatal("--trace changed the comparison", code, f.diagnostic.String())
	}
	data, err := os.ReadFile(traceFile)
	var trace traceDocument
	if err != nil || json.Unmarshal(data, &trace) != nil || trace.Schema != TraceSchema || trace.Status != "partial_failure" || trace.Revisions.Head != headSHA || trace.Dropped != 0 {
		t.Fatalf("trace document: %v %s", err, data)
	}
	if info, statErr := os.Stat(traceFile); statErr != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatal("trace file is not private", statErr)
	}
	assertNoSecrets(t, "trace", string(data), "fictional-", "Bearer", f.gitURL, f.argoURL)
	// The template failure is located at the parent's Helm step in the
	// discovery probe on both sides, and the final walk shows that it reused
	// that result instead of rendering again.
	located := map[string]bool{}
	for _, failure := range trace.Failures {
		if failure.Application == nil || failure.Application.Name != "parent" || failure.Outcome != "partial" || failure.Code == "" {
			continue
		}
		if failure.Walk == "probe" && failure.Phase == "render" && failure.Step == "helm" {
			located["probe "+failure.Side] = true
		}
		if failure.Walk == "final" && failure.Phase == "render" && failure.Step == "expansion-cache" {
			located["final "+failure.Side] = true
		}
		if failure.Phase == "http" {
			t.Fatal("a handled request failure was listed as the failing step")
		}
	}
	if len(located) != 4 {
		t.Fatalf("failing Helm step not located on both sides and walks: %+v", trace.Failures)
	}
	var source, http, lastSeq = false, false, 0
	for _, step := range trace.Steps {
		if step.Seq != lastSeq+1 {
			t.Fatal("trace steps are not in completion order")
		}
		lastSeq = step.Seq
		source = source || (step.Phase == "source" && step.Step == "files" && step.Outcome == "ok" && strings.HasPrefix(step.Subject, "example/charts@"))
		http = http || (step.Phase == "http" && step.Subject == "/api/v1/projects/{project}" && step.Status == 200)
	}
	if !source || !http {
		t.Fatal("trace lost the source read or the provider request step")
	}
	// Identical inputs produce identical digests in another run.
	digests := func(trace traceDocument) map[string]bool {
		out := map[string]bool{}
		for _, step := range trace.Steps {
			if step.Digest != "" && step.Application != nil {
				out[step.Application.String()+" "+step.Side+" "+step.Phase+"/"+step.Step+" "+step.Subject+" "+step.Digest] = true
			}
		}
		return out
	}
	first := digests(trace)
	for _, want := range []string{"lookup/spec", "source/render-inputs", "render/helm", "project/spec", "context/settings", "context/capabilities"} {
		found := false
		for key := range first {
			found = found || strings.Contains(key, " "+want+" ")
		}
		if !found {
			t.Fatalf("no %s digest in the trace", want)
		}
	}
	if code = f.run("diff", "--config", f.config, "--pr", "12", "--format", "json", "--trace", traceFile, "--quiet"); code != 3 {
		t.Fatal("second traced run", code)
	}
	data, _ = os.ReadFile(traceFile)
	var again traceDocument
	if json.Unmarshal(data, &again) != nil || !reflect.DeepEqual(first, digests(again)) {
		t.Fatal("identical inputs produced different trace digests")
	}
}
