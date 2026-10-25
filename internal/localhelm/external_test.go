package localhelm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
	"diffaro/internal/manifest"
	"go.yaml.in/yaml/v3"
)

// The test executable is also a portable fake Helm CLI. No shell scripts or
// production credentials are required by unit tests.
func init() {
	if len(os.Args) < 2 || (os.Args[1] != "version" && os.Args[1] != "template") {
		return
	}
	for _, key := range []string{"DIFFARO_EXTERNAL_TEST_SECRET", "DIFFARO_GITHUB_TOKEN", "DIFFARO_ARGOCD_TOKEN", "PATH"} {
		if os.Getenv(key) != "" {
			os.Exit(20)
		}
	}
	if os.Args[1] == "version" {
		fmt.Println("v3.19.0+fixture")
		os.Exit(0)
	}
	if counter := os.Getenv("FICTIONAL_RENDER_COUNTER"); counter != "" {
		f, err := os.OpenFile(counter, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(23)
		}
		f.WriteString("x")
		f.Close()
	}
	// A render waits until the gate file exists, so a test can start another
	// render while this one runs.
	if gate := os.Getenv("FICTIONAL_RENDER_GATE"); gate != "" {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(gate); err == nil {
				break
			}
		}
	}
	var filename, namespace string
	for i, a := range os.Args {
		if i+1 < len(os.Args) {
			if a == "--values" {
				filename = os.Args[i+1]
			}
			if a == "--namespace" {
				namespace = os.Args[i+1]
			}
		}
	}
	var values map[string]any
	if len(os.Args) < 4 {
		os.Exit(21)
	}
	data, err := os.ReadFile(filepath.Join(os.Args[3], "values.yaml"))
	if err != nil || yaml.Unmarshal(data, &values) != nil {
		os.Exit(21)
	}
	if filename != "" {
		data, err = os.ReadFile(filename)
		var overrides map[string]any
		if err != nil || yaml.Unmarshal(data, &overrides) != nil {
			os.Exit(21)
		}
		for key, value := range overrides {
			values[key] = value
		}
	}
	if os.Getenv("KUBECONFIG") == "" {
		os.Exit(21)
	}
	switch values["externalTestCase"] {
	case "located":
		fmt.Fprintln(os.Stderr, "Error: execution error at (app/templates/deployment.yaml:7:3): fictional-private-value")
		os.Exit(1)
	case "fail":
		fmt.Fprintln(os.Stderr, "fictional-private-value")
		fmt.Println("fictional-private-value")
		os.Exit(22)
	case "timeout":
		time.Sleep(time.Minute)
	case "oversize":
		fmt.Print(strings.Repeat("fictional-private-value", 10000))
		os.Exit(0)
	case "warnings":
		// Harmless Helm warnings far past the 64 KiB diagnostics bound.
		for i := 0; i < 800; i++ {
			fmt.Fprintf(os.Stderr, "coalesce.go:286: warning: cannot overwrite table with non table for fictional.key%d (map[])\n", i)
		}
	case "padded":
		// Raw output larger than render.maxResponseBytes whose targets fit.
		fmt.Print(strings.Repeat("# Source: app/templates/padding.yaml\n", 256))
	case "flood":
		fmt.Print(strings.Repeat("# fictional\n", 200000))
		os.Exit(0)
	case "nondeterministic":
		values["replicas"] = time.Now().UnixNano()
	case "nondeterministic-name":
		fmt.Printf("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: job-%d, namespace: %q}\n---\n", time.Now().UnixNano(), namespace)
	case "report-subchart":
		// Report whether the chart handed to the executable still holds
		// its vendored db subchart (SaveDir writes it as an archive).
		values["replicas"] = "absent"
		if _, err := os.Stat(filepath.Join(os.Args[3], "charts", "db-0.1.0.tgz")); err == nil {
			values["replicas"] = "present"
		}
	case "hook":
		fmt.Printf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: hooked\n  namespace: %q\n  annotations: {helm.sh/hook: %q}\n---\n", namespace, fmt.Sprint(values["hook"]))
	case "environment":
		// Echo only the allow-listed variable so the test can prove delivery.
		values["replicas"] = os.Getenv("FICTIONAL_METADATA_HOST")
	}
	if values["replicas"] == nil {
		values["replicas"] = 2
	}
	fmt.Printf("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: api, namespace: %q}\ndata: {replicas: %q}\n", namespace, fmt.Sprint(values["replicas"]))
	os.Exit(0)
}

func fakeExternal(t *testing.T) *External {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Invoke the already running test image for subprocess protocol tests.
	// Snapshot copying is tested separately; official CLI tests cover probing
	// and execution of copied binaries without duplicating a large test image.
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	e := &External{directory: t.TempDir(), binary: binary, Version: "v3.19.0+fixture", SHA256: hex.EncodeToString(hash[:])}
	t.Cleanup(e.Close)
	return e
}

func TestExternalHelmIsolationFrozenIdentityAndFailures(t *testing.T) {
	t.Setenv("DIFFARO_EXTERNAL_TEST_SECRET", "fictional-token")
	t.Setenv("DIFFARO_GITHUB_TOKEN", "fictional-pat")
	t.Setenv("KUBECONFIG", "must-not-be-read")
	e := fakeExternal(t)
	binary, _ := os.Executable()
	data, _ := os.ReadFile(binary)
	hash := sha256.Sum256(data)
	if e.SHA256 != hex.EncodeToString(hash[:]) || e.Version != "v3.19.0+fixture" {
		t.Fatal("untraceable executable")
	}
	info := (&Client{API: &argocd.Client{Instance: "east"}, External: e}).BackendInfo()
	if info.HelmVersion != e.Version || info.HelmSHA256 != e.SHA256 || info.Verification != "local-context-unverified" {
		t.Fatal("backend identity lost or certified without evidence")
	}
	req := fixtureRequest()
	got, err := e.Render(context.Background(), req)
	if err != nil || len(got) != 1 || !strings.Contains(got[0], `"replicas":"2"`) || !strings.Contains(got[0], "tracking-id") {
		t.Fatal("inputs/tracking lost", err, got)
	}
	for _, scenario := range []string{"fail", "located", "oversize", "nondeterministic", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			req := fixtureRequest()
			req.Plan[0].Source["helm"].(map[string]any)["valuesObject"] = map[string]any{"externalTestCase": scenario}
			timeout := 10 * time.Second
			if scenario == "timeout" {
				timeout = 2 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			out, err := e.Render(ctx, req)
			if err == nil || len(out) != 0 || strings.Contains(err.Error(), "fictional-") {
				t.Fatal("failure hidden or secret exposed", err)
			}
			var stopped *domain.Failure
			if scenario == "timeout" && (!errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &stopped) || stopped.Issue.Reason != "external Helm render did not finish") {
				t.Fatal("subprocess timeout lost or unnamed", err)
			}
			if scenario == "located" {
				var f *domain.Failure
				if !errors.As(err, &f) || !strings.Contains(f.Issue.Reason, "template app/templates/deployment.yaml:7 failed: the chart stopped rendering with fail or required") {
					t.Fatal("external template failure location lost", err)
				}
			}
			if scenario == "nondeterministic" {
				var f *domain.Failure
				if !errors.As(err, &f) || f.Issue.Code != "render_nondeterministic" {
					t.Fatal("non-determinism not explicit", err)
				}
			}
		})
	}
	temp := e.directory
	e.Close()
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatal("inline values or executable snapshot retained")
	}
}

// External Helm output bounds keep memory bounded without refusing renders
// the targets limit accepts: diagnostics past 64 KiB are truncated, raw
// output may exceed render.maxResponseBytes by its YAML, comments and hooks,
// and output past the stdout bound fails as limit_exceeded naming the setting.
func TestExternalHelmOutputBounds(t *testing.T) {
	e := fakeExternal(t)
	for _, scenario := range []string{"warnings", "padded"} {
		req := fixtureRequest()
		req.Limit = 4096
		req.Plan[0].Source["helm"].(map[string]any)["valuesObject"] = map[string]any{"externalTestCase": scenario}
		got, err := e.Render(context.Background(), req)
		if err != nil || len(got) != 1 || !strings.Contains(got[0], `"replicas":"2"`) {
			t.Fatalf("%s: render refused by an output bound: %v %v", scenario, err, got)
		}
	}
	req := fixtureRequest()
	req.Limit = 1024
	req.Plan[0].Source["helm"].(map[string]any)["valuesObject"] = map[string]any{"externalTestCase": "flood"}
	_, err := e.Render(context.Background(), req)
	var failure *domain.Failure
	if !errors.As(err, &failure) || failure.Issue.Code != domain.CodeLimitExceeded || !strings.Contains(failure.Issue.Reason, "render.maxResponseBytes") || !strings.Contains(failure.Issue.Action, "render.maxResponseBytes") {
		t.Fatal("stdout past its bound not reported as limit_exceeded naming render.maxResponseBytes", err)
	}
}

// A stopped version probe names the version check, not a render, and its
// own limit is not reported as render.timeout.
func TestExternalHelmStoppedVersionProbeIsNotARender(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = NewExternal(cancelled, binary)
	var failure *domain.Failure
	if !errors.Is(err, context.Canceled) || !errors.As(err, &failure) || failure.Issue.Code != domain.CodeCancelled || failure.Issue.Reason != "external Helm version check did not finish" {
		t.Fatal("cancelled probe reported as a render", err)
	}
	limit := versionProbeTimeout
	t.Cleanup(func() { versionProbeTimeout = limit })
	versionProbeTimeout = 0
	_, err = NewExternal(context.Background(), binary)
	if errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &failure) || failure.Issue.Code != domain.CodeHelmExecutionFailed || !strings.Contains(failure.Issue.Reason, "did not answer version --short within") || strings.Contains(failure.Issue.Action, "render.timeout") {
		t.Fatal("probe timeout reported as a render.timeout", err)
	}
}

func TestExternalHelmMissingExecutableAndUnsafeTemplates(t *testing.T) {
	if _, err := NewExternal(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing executable accepted")
	}
	script := filepath.Join(t.TempDir(), "helm-script")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho v3.19.0"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewExternal(context.Background(), script); err == nil {
		t.Fatal("wrapper script accepted as native Helm")
	}
	e := fakeExternal(t)
	// A chart that names a time, DNS or small-range function renders
	// externally only when the bundled engine renders the same chart and
	// values completely without calling one. Random-output
	// functions are caught by the double render instead; the fake CLI's
	// nondeterministic cases stand in for a call that is made. A field that
	// differs is marked and not compared; a resource set that differs skips
	//.
	subchart := map[string][]byte{
		"app/charts/db/Chart.yaml":            []byte("apiVersion: v2\nname: db\nversion: 0.1.0\n"),
		"app/charts/db/templates/secret.yaml": []byte("{{- if not .Values.password }}\napiVersion: v1\nkind: Secret\nmetadata: {name: db}\nstringData: {password: {{ randAlphaNum 10 | quote }}}\n{{- end }}\n"),
	}
	for _, tc := range []struct {
		name     string
		files    map[string][]byte
		values   string
		want     string
		unstable bool
	}{
		{"inactive reference renders", map[string][]byte{"app/templates/config.yaml": []byte(`{{ if false }}{{ now }}{{ end }}`)}, "", "", false},
		{"subchart branch switched off renders", subchart, "db: {password: fixed}\n", "", false},
		{"subchart branch switched on marks the differing field", subchart, "externalTestCase: nondeterministic\n", "", true},
		{"random resource name skips", subchart, "externalTestCase: nondeterministic-name\n", "external Helm templates call randomness functions: two renders of identical inputs produced different resources", false},
		{"active call skips", map[string][]byte{"app/templates/config.yaml": []byte(`# {{ now }}`)}, "", "template app/templates/config.yaml:1 invokes randomness", false},
		{"value template call marks the differing field", map[string][]byte{"app/templates/config.yaml": []byte(`# {{ tpl .Values.generated . }}`)}, "generated: '{{ uuidv4 }}'\nexternalTestCase: nondeterministic\n", "", true},
		{"random-only chart the bundled engine cannot render renders", map[string][]byte{"app/templates/config.yaml": []byte(`{{ if false }}{{ randAlphaNum 10 }}{{ end }}{{ mustExampleIPs "x" }}`)}, "", "", false},
		{"small-range integer still needs the probe", map[string][]byte{"app/templates/config.yaml": []byte(`{{ if false }}{{ randInt 0 2 }}{{ end }}{{ mustExampleIPs "x" }}`)}, "", "could not render the chart to rule out a call", false},
		{"chart the bundled engine cannot render skips", map[string][]byte{"app/templates/config.yaml": []byte(`{{ if false }}{{ now }}{{ end }}{{ mustExampleIPs "x" }}`)}, "", `could not render the chart to rule out a call: template app/templates/config.yaml:1 uses unavailable function "mustExampleIPs"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := fixtureRequest()
			files := req.Repositories[testRepo+"@"+testSHA]
			for name, data := range tc.files {
				files[name] = data
			}
			if tc.values != "" {
				files["app/values.yaml"] = append(files["app/values.yaml"], tc.values...)
			}
			docs, err := e.Render(context.Background(), req)
			var skipped *domain.RenderSkipped
			switch {
			case tc.want == "" && err != nil:
				t.Fatal("a chart whose functions are not called did not render", err)
			case tc.want != "" && (!errors.As(err, &skipped) || !strings.Contains(skipped.Reason, tc.want)):
				t.Fatalf("got %v, want a skip mentioning %q", err, tc.want)
			}
			if tc.want != "" {
				return
			}
			targets, err := manifest.Parse(docs, "east", "demo", manifest.Limits{Bytes: 1 << 20, Documents: 100}, nil)
			if err != nil {
				t.Fatal(err)
			}
			target, ok := targets[(domain.ResourceID{Cluster: "east", Kind: "ConfigMap", Namespace: "demo", Name: "api"}).String()]
			if !ok {
				t.Fatalf("the rendered ConfigMap is missing: %v", docs)
			}
			replicas := target.Object["data"].(map[string]any)["replicas"]
			if tc.unstable != (replicas == domain.RenderUnstable) {
				t.Fatalf("replicas = %v; want marked as differing between renders: %t", replicas, tc.unstable)
			}
		})
	}
	req := fixtureRequest()
	var skipped *domain.RenderSkipped
	req.Repositories[testRepo+"@"+testSHA]["app/templates/lookup.yaml"] = []byte(`{{ if lookup "v1" "Secret" "demo" "token" }}{{ end }}`)
	if _, err := e.Render(context.Background(), req); errors.As(err, &skipped) {
		t.Fatal("lookup, which helm template answers without a cluster, was skipped", err)
	}
}

func TestExternalHelmRealCLIEquivalence(t *testing.T) {
	binary := os.Getenv("DIFFARO_TEST_HELM_BINARY")
	if binary == "" {
		t.Skip("set DIFFARO_TEST_HELM_BINARY to a checksum-verified Helm CLI")
	}
	e, err := NewExternal(context.Background(), binary)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, scenario := range []string{"defaults", "all-inputs", "external-values", "file-dependency"} {
		t.Run(scenario, func(t *testing.T) {
			req := fixtureRequest()
			files := req.Repositories[testRepo+"@"+testSHA]
			helm := req.Plan[0].Source["helm"].(map[string]any)
			switch scenario {
			case "all-inputs":
				files["app/templates/types.yaml"] = []byte(`apiVersion: v1
kind: ConfigMap
metadata: {name: types}
data: {parameterType: {{ kindOf .Values.replicas | quote }}}
`)
				files["app/override.yaml"] = []byte("replicas: 4\n")
				files["app/message.txt"] = []byte("file-secret\n")
				helm["valueFiles"] = []any{"override.yaml"}
				helm["values"] = "replicas: 5\n"
				helm["valuesObject"] = map[string]any{"replicas": 6}
				helm["parameters"] = []any{map[string]any{"name": "replicas", "value": "9"}, map[string]any{"name": "message", "value": "10", "forceString": true}}
				helm["fileParameters"] = []any{map[string]any{"name": "message", "path": "message.txt"}}
				helm["releaseName"] = "renamed"
				helm["namespace"] = "explicit"
			case "external-values":
				repository := "https://github.com/example/values"
				req.Plan = append(req.Plan, domain.SourceRevision{Position: 2, Repository: repository, Revision: testSHA, Ref: "values", Source: map[string]any{"repoURL": repository, "ref": "values", "targetRevision": testSHA}})
				req.Repositories[repository+"@"+testSHA] = map[string][]byte{"prod.yaml": []byte("replicas: 8\n")}
				helm["valueFiles"] = []any{"$values/prod.yaml"}
			case "file-dependency":
				files["app/Chart.yaml"] = []byte("apiVersion: v2\nname: app\nversion: 0.1.0\ndependencies:\n  - name: shared\n    version: 0.1.0\n    repository: file://../shared\n")
				files["shared/Chart.yaml"] = []byte("apiVersion: v2\nname: shared\nversion: 0.1.0\n")
				files["shared/templates/config.yaml"] = []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: shared}\ndata: {value: stable}\n")
			}
			want, err := Render(req)
			if err != nil {
				t.Fatal(err)
			}
			got, err := e.Render(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(want)
			sort.Strings(got)
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("external target differs from bundled Helm: want %v got %v", want, got)
			}
		})
	}
}

func TestExternalHelmOriginalReplacementDoesNotChangeSnapshot(t *testing.T) {
	// A small native-header fixture exercises freezing independently of the
	// platform's potentially large race-instrumented test executable.
	data := append([]byte{'M', 'Z', 0, 0}, bytes.Repeat([]byte{'A'}, 1024)...)
	original := filepath.Join(t.TempDir(), "operator-helm.exe")
	if err := os.WriteFile(original, data, 0700); err != nil {
		t.Fatal(err)
	}
	e, err := snapshotExecutable(original)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err = os.WriteFile(original, []byte("replacement must not be executed"), 0700); err != nil {
		t.Fatal(err)
	}
	frozen, err := os.ReadFile(e.binary)
	if err != nil || !bytes.Equal(data, frozen) {
		t.Fatal("original executable replacement changed frozen bytes", err)
	}
}

func TestExternalHelmRealCLIPartialTargets(t *testing.T) {
	binary := os.Getenv("DIFFARO_TEST_HELM_BINARY")
	if binary == "" {
		t.Skip("set DIFFARO_TEST_HELM_BINARY")
	}
	e, err := NewExternal(context.Background(), binary)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	req := fixtureRequest()
	files := req.Repositories[testRepo+"@"+testSHA]
	files["app/templates/broken.yaml"] = []byte(`{{ mustExampleIPs "fictional-private-value" }}`)
	want, wantErr := Render(req)
	out, err := e.Render(context.Background(), req)
	if !domain.IsRenderPartial(wantErr) || !domain.IsRenderPartial(err) || !reflect.DeepEqual(want, out) || !strings.Contains(err.Error(), "broken.yaml:1") || strings.Contains(err.Error(), "fictional-private-value") {
		t.Fatalf("external partial mismatch: %v %v / %v %v", want, wantErr, out, err)
	}
	files["app/templates/broken.yaml"] = []byte(`{{ set .Values "message" "wrong" }}{{ mustExampleIPs "demo" }}`)
	out, err = e.Render(context.Background(), req)
	if len(out) != 0 || err == nil || domain.IsRenderPartial(err) {
		t.Fatal("unsafe external isolation", err)
	}
}

func TestExternalHelmReceivesOnlyAllowListedEnvironment(t *testing.T) {
	t.Setenv("DIFFARO_EXTERNAL_TEST_SECRET", "fictional-token")
	t.Setenv("FICTIONAL_METADATA_HOST", "inherited-must-not-be-used")
	e := fakeExternal(t)
	req := fixtureRequest()
	req.Plan[0].Source["helm"].(map[string]any)["valuesObject"] = map[string]any{"externalTestCase": "environment"}
	got, err := e.Render(context.Background(), req)
	if err != nil || len(got) != 1 || !strings.Contains(got[0], `"replicas":""`) {
		t.Fatal("process environment leaked into external Helm", err, got)
	}
	e.Environment = []string{"FICTIONAL_METADATA_HOST=https://metadata.example.invalid"}
	got, err = e.Render(context.Background(), req)
	if err != nil || len(got) != 1 || !strings.Contains(got[0], `"replicas":"https://metadata.example.invalid"`) {
		t.Fatal("configured environment not delivered", err, got)
	}
	info := (&Client{API: &argocd.Client{Instance: "east"}, External: e}).BackendInfo()
	if !reflect.DeepEqual(info.HelmEnvironment, []string{"FICTIONAL_METADATA_HOST"}) || info.Verification != "local-context-unverified" {
		t.Fatal("environment provenance must list names only and stay unverified", info)
	}
}

func TestExternalHelmChecksIdenticalInputsOnce(t *testing.T) {
	e := fakeExternal(t)
	counter := filepath.Join(t.TempDir(), "renders")
	e.Environment = []string{"FICTIONAL_RENDER_COUNTER=" + counter}
	renders := func() int {
		data, _ := os.ReadFile(counter)
		return len(data)
	}
	first, err := e.Render(context.Background(), fixtureRequest())
	if err != nil || renders() != 2 {
		t.Fatal("first render must run twice to check determinism", err, renders())
	}
	// Base and head of an unaffected Application: identical inputs reuse the
	// verified output, rendered in a different isolated directory.
	second, err := e.Render(context.Background(), fixtureRequest())
	if err != nil || renders() != 2 || !reflect.DeepEqual(first, second) {
		t.Fatal("identical inputs were rendered again", err, renders())
	}
	changed := fixtureRequest()
	changed.Plan[0].Source["helm"].(map[string]any)["valuesObject"] = map[string]any{"replicas": 3}
	got, err := e.Render(context.Background(), changed)
	if err != nil || renders() != 4 || reflect.DeepEqual(got, first) {
		t.Fatal("changed inputs must be rendered and checked", err, renders())
	}
	nondeterministic := fixtureRequest()
	nondeterministic.Plan[0].Source["helm"].(map[string]any)["valuesObject"] = map[string]any{"externalTestCase": "nondeterministic"}
	for range 2 {
		var f *domain.Failure
		if _, err := e.Render(context.Background(), nondeterministic); !errors.As(err, &f) || f.Issue.Code != "render_nondeterministic" {
			t.Fatal("a failed determinism check must not be reused", err)
		}
	}
	if renders() != 8 {
		t.Fatal("failed renders must be repeated", renders())
	}
}

// Base and head of an unaffected Application render concurrently: the second
// render of identical inputs waits for the running one and reuses its
// verified output, but never its failed determinism check.
func TestExternalHelmConcurrentIdenticalInputsRenderOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		values  map[string]any
		renders int
		code    string
	}{
		{"verified output is shared", nil, 2, ""},
		{"failed determinism check is repeated", map[string]any{"externalTestCase": "nondeterministic"}, 4, "render_nondeterministic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := fakeExternal(t)
			dir := t.TempDir()
			counter, gate := filepath.Join(dir, "renders"), filepath.Join(dir, "gate")
			e.Environment = []string{"FICTIONAL_RENDER_COUNTER=" + counter, "FICTIONAL_RENDER_GATE=" + gate}
			renders := func() int {
				data, _ := os.ReadFile(counter)
				return len(data)
			}
			req := func() Request {
				r := fixtureRequest()
				if tc.values != nil {
					r.Plan[0].Source["helm"].(map[string]any)["valuesObject"] = tc.values
				}
				return r
			}
			errs := make(chan error, 2)
			outputs := make(chan []string, 2)
			start := func() {
				go func() {
					out, err := e.Render(context.Background(), req())
					outputs <- out
					errs <- err
				}()
			}
			waitFor := func(n int, limit time.Duration) {
				for deadline := time.Now().Add(limit); renders() < n && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
				}
			}
			start()
			waitFor(1, 10*time.Second)
			if renders() != 1 {
				t.Fatal("first render did not start", renders())
			}
			// The second render starts while the first waits at the gate. A
			// second helm run within this time would be a duplicate render.
			start()
			waitFor(2, 500*time.Millisecond)
			if err := os.WriteFile(gate, nil, 0600); err != nil {
				t.Fatal(err)
			}
			var got [][]string
			for range 2 {
				err := <-errs
				got = append(got, <-outputs)
				var f *domain.Failure
				if tc.code == "" && err != nil || tc.code != "" && (!errors.As(err, &f) || f.Issue.Code != tc.code) {
					t.Fatalf("render result %v, want code %q", err, tc.code)
				}
			}
			if renders() != tc.renders {
				t.Fatalf("helm template ran %d times, want %d", renders(), tc.renders)
			}
			if tc.code == "" && (len(got[0]) == 0 || !reflect.DeepEqual(got[0], got[1])) {
				t.Fatal("the waiting render did not reuse the verified output", got)
			}
		})
	}
}

func TestExternalVersionAcceptsHelm3And4WithOrWithoutPrefix(t *testing.T) {
	for version, want := range map[string]bool{
		"v3.19.0+fixture":       true,
		"3.12.0-acme1+gabc1234": true,
		"v4.0.1":                true,
		"v2.17.0":               false,
		"2.17.0":                false,
		"Client: v3.12.0":       false,
		"version.BuildInfo{}":   false,
		"":                      false,
	} {
		if got := externalVersion.MatchString(version); got != want {
			t.Fatalf("version %q accepted=%t, want %t", version, got, want)
		}
	}
}

// An Application that requests Helm v4 is skipped on a Helm 3 executable,
// whether or not the executable's version starts with "v".
func TestHelmV4RequestSkipsOnHelm3WithOrWithoutPrefix(t *testing.T) {
	for version, skips := range map[string]bool{"v3.19.0+fixture": true, "3.12.0-acme1+gabc1234": true, "v4.0.1": false, "4.0.1": false} {
		e := fakeExternal(t)
		e.Version = version
		req := fixtureRequest()
		req.Plan[0].Source["helm"].(map[string]any)["version"] = "v4"
		_, err := e.Render(context.Background(), req)
		var skipped *domain.RenderSkipped
		if got := errors.As(err, &skipped) && strings.Contains(skipped.Reason, "requests Helm v4"); got != skips {
			t.Fatalf("version %q: skipped for Helm v4 = %t (err %v), want %t", version, got, err, skips)
		}
	}
}
