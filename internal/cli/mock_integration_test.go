package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/reporter"
	"diffaro/internal/reposerver"
	"diffaro/internal/security"
)

func mockRequestContext() map[string]any {
	return map[string]any{
		"repo":  map[string]any{"repo": fakeRepo, "password": "fictional-repository-secret"},
		"repos": []any{}, "helmRepoCreds": []any{}, "helmOptions": map[string]any{},
		"kubeVersion": "1.32.0", "apiVersions": []any{"v1", "apps/v1", "argoproj.io/v1alpha1"},
		"appLabelKey": "app.kubernetes.io/instance", "trackingMethod": "annotation", "installationID": "",
		"enabledSourceTypes": map[string]any{"Helm": true, "Plugin": false},
	}
}

func mockChild(name, revision string) map[string]any {
	a := recursiveApp(name)
	if name == "child" && revision == headSHA {
		helm := a["spec"].(map[string]any)["source"].(map[string]any)["helm"].(map[string]any)
		helm["releaseName"] = "child-updated"
		helm["parameters"] = []any{map[string]any{"name": "replicas", "value": "5", "forceString": true}}
		helm["valuesObject"] = map[string]any{"password": "fictional-object-value", "replicas": json.Number("4")}
	}
	return a
}

// mockRPC is the fake grpcurl for mockConfig (roles rpcChart and
// rpcRecursive in main_test.go).
func mockRPC(recursive bool) error {
	for _, key := range []string{"DIFFARO_TEST_GITHUB", "DIFFARO_TEST_ARGO_0"} {
		if os.Getenv(key) != "" {
			return fmt.Errorf("API token inherited by repo-server process")
		}
	}
	if strings.Contains(strings.Join(os.Args[1:], " "), "fictional") {
		return fmt.Errorf("credential appeared in argv")
	}
	var request map[string]any
	d := json.NewDecoder(os.Stdin)
	d.UseNumber()
	if err := d.Decode(&request); err != nil {
		return err
	}
	revision, _ := request["revision"].(string)
	if revision != baseSHA && revision != headSHA {
		return fmt.Errorf("RPC did not receive fixed SHA")
	}
	name, _ := request["appName"].(string)
	if name != "api" && name != "child" && name != "new-child" {
		return fmt.Errorf("unexpected Application")
	}
	expectedApp := testApp()
	if recursive {
		expectedApp = mockChild(name, revision)
	}
	expectedSource := expectedApp["spec"].(map[string]any)["source"].(map[string]any)
	expectedSource["targetRevision"] = revision
	actualSource, _ := request["applicationSource"].(map[string]any)
	helm, _ := actualSource["helm"].(map[string]any)
	values, _ := helm["valuesObject"].(map[string]any)
	raw, err := base64.StdEncoding.DecodeString(domain.String(values, "raw"))
	if err != nil {
		return fmt.Errorf("valuesObject wire format lost")
	}
	var decoded map[string]any
	d = json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	if err = d.Decode(&decoded); err != nil {
		return fmt.Errorf("valuesObject is not valid JSON")
	}
	helm["valuesObject"] = decoded
	if !reflect.DeepEqual(actualSource, expectedSource) {
		return fmt.Errorf("full desired Helm inputs were not preserved")
	}
	if request["namespace"] != "demo" || request["projectName"] != "demo" || request["noCache"] != true || request["noRevisionCache"] != true {
		return fmt.Errorf("render context missing")
	}
	for key, value := range mockRequestContext() {
		if !reflect.DeepEqual(request[key], value) {
			return fmt.Errorf("trusted context missing: %s", key)
		}
	}
	manifests := []string{}
	if recursive && name == "api" {
		raw, _ := json.Marshal(mockChild("child", revision))
		manifests = append(manifests, string(raw))
		if revision == headSHA {
			raw, _ = json.Marshal(mockChild("new-child", revision))
			manifests = append(manifests, string(raw))
		}
	} else {
		replicas := 2
		if revision == headSHA {
			replicas = 3
		}
		if name == "child" && revision == headSHA {
			replicas = 5
		}
		if name == "new-child" {
			replicas = 1
		}
		manifests = append(manifests, fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":%q},"spec":{"replicas":%d}}`, name, replicas))
	}
	return json.NewEncoder(os.Stdout).Encode(reposerver.Response{Revision: revision, SourceType: "Helm", Manifests: manifests})
}

func mockConfig(t *testing.T, recursive, allowNew bool) string {
	t.Helper()
	g := gitServer(t)
	a := readOnlyFake(t, "Argo CD", "fictional-argo-0", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"Version":"v3.5.3"}`)
		case "/api/v1/projects/demo":
			fmt.Fprint(w, `{"metadata":{"name":"demo"},"spec":{"sourceRepos":["https://github.com/example/charts.git"],"destinations":[{"name":"test-cluster","namespace":"demo"}]}}`)
		case "/api/v1/applications/api":
			app := testApp()
			if recursive {
				app = recursiveApp("api")
			}
			_ = json.NewEncoder(w).Encode(app)
		default:
			t.Error("desired render must not look up installed children or use stored-spec manifests")
			http.NotFound(w, r)
		}
	})
	file := configFile(t, g.URL, a.URL)
	names := []string{"api", "child"}
	if allowNew {
		names = nil // Newly discovered children need no name registration.
	}
	snapshot := reposerver.Snapshot{Version: "v1alpha1", Instance: "instance-0", ContextID: "mock-context-v1", ArgoCDNamespace: "argocd", Bindings: []reposerver.Binding{{
		Project: "demo", ApplicationNamespace: "argocd", Applications: names, RepositoryURL: fakeRepo,
		Destination: map[string]any{"name": "test-cluster", "namespace": "demo"}, Request: mockRequestContext(),
	}}}
	data, _ := json.Marshal(snapshot)
	if err := os.WriteFile(filepath.Join(filepath.Dir(file), "context.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	role, port := rpcChart, 19001
	if recursive {
		role, port = rpcRecursive, 19002
	}
	quoted, _ := json.Marshal(fakeGRPCURL(t, role))
	block := fmt.Sprintf("    tokenEnv: DIFFARO_TEST_ARGO_0\n    repoServer:\n      address: 127.0.0.1:%d\n      plaintext: true\n      grpcurl: %s\n      contextFile: context.json", port, quoted)
	editProfile(t, file, "    tokenEnv: DIFFARO_TEST_ARGO_0", block)
	if recursive {
		appendProfile(t, file, "routing:\n  defaultInstance: instance-0\n")
	}
	return file
}

func TestMockChartOnlyPreview(t *testing.T) {
	report, code := runJSON(t, mockConfig(t, false, true), "--base", "main", "--head", "feature")
	if code != 1 || report.Status != "success" || len(report.Applications) != 1 {
		t.Fatalf("mock chart preview failed: code=%d issues=%+v", code, report.Applications)
	}
	changes := report.Applications[0].Resources[0].Changes
	if len(changes) != 1 || changes[0].Path != "/spec/replicas" || fmt.Sprint(changes[0].Before) != "2" || fmt.Sprint(changes[0].After) != "3" {
		t.Fatal("target diff missing")
	}
	if len(report.RenderBackends) != 1 || report.RenderBackends[0].ContextID != "mock-context-v1" || report.RenderBackends[0].Verification != "live-unverified" {
		t.Fatal("backend provenance missing")
	}
}

func TestMockPRMarkdownOutputFile(t *testing.T) {
	config := mockConfig(t, false, true)
	file := filepath.Join(t.TempDir(), "comment.md")
	jsonFile := filepath.Join(filepath.Dir(file), "report.json")
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"diff", "--config", config, "--base", "main", "--head", "feature", "--format", "pr-markdown", "--output", file, "--json-output", jsonFile, "--quiet"}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("PR artifact failed: code=%d diagnostic=%s", code, stderr.String())
	}
	data, err := os.ReadFile(file)
	if err != nil || !bytes.HasPrefix(data, []byte("<!-- kubeseer:preview -->")) || !bytes.Contains(data, []byte("/spec/replicas")) {
		t.Fatal("PR artifact did not preserve the target diff", err)
	}
	assertNoSecrets(t, "PR artifact", string(data), "fictional-")
	jsonData, err := os.ReadFile(jsonFile)
	if err != nil {
		t.Fatal("machine report missing", err)
	}
	assertNoSecrets(t, "machine report", string(jsonData), "fictional-")
	var report domain.Report
	if err = json.Unmarshal(jsonData, &report); err != nil || report.Status != "success" || len(report.Revisions.Base) != 40 || len(report.Revisions.Head) != 40 || !bytes.Contains(data, []byte(report.Revisions.Head)) {
		t.Fatal("PR and JSON artifacts did not use the same resolved comparison", err)
	}
}

func TestRejectSameArtifactPath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "report.md")
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"diff", "--format", "pr-markdown", "--output", file, "--json-output", file}, &stdout, &stderr)
	if code != 4 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "different paths") {
		t.Fatal("same output path should fail before rendering")
	}
}

func TestMockRecursivePreview(t *testing.T) {
	for _, allowNew := range []bool{true, false} {
		t.Run(fmt.Sprintf("new-child-authorized-%t", allowNew), func(t *testing.T) {
			report, code := runJSON(t, mockConfig(t, true, allowNew), "--base", "main", "--head", "feature", "--recursive")
			if report.Graph == nil || len(report.Applications) != 3 {
				t.Fatalf("mock graph missing: code=%d", code)
			}
			apps := map[string]domain.ApplicationResult{}
			for _, app := range report.Applications {
				apps[app.ID.Name] = app
			}
			child := apps["child"]
			if child.Execution != "complete" || child.State != "changed" || child.HeadTarget.Inputs.ReleaseName != "child-updated" || len(child.Resources) != 1 || fmt.Sprint(child.Resources[0].Changes[0].After) != "5" {
				t.Fatalf("modified child did not use desired inputs: %+v", child)
			}
			added := apps["new-child"]
			if !allowNew {
				if code != 3 || added.Execution != "denied" || added.Issues[0].Code != "access_denied" {
					t.Fatal("denied child hid independent branch or bypassed binding")
				}
				return
			}
			if code != 1 || added.State != "added" || added.Execution != "complete" || len(added.Resources) != 1 || added.Resources[0].State != "added" {
				t.Fatalf("new child was not rendered: code=%d result=%+v", code, added)
			}
			for _, format := range []string{"json", "markdown", "pr-markdown"} {
				actual, err := reporter.Encode(report, format, security.Redactor{})
				if err != nil {
					t.Fatal(err)
				}
				assertNoSecrets(t, format+" report", string(actual), "fictional-")
			}
		})
	}
}
