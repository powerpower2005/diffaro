package reposerver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
	"diffaro/internal/httpx"
	"diffaro/internal/profile"
)

const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const repository = "https://github.com/example/charts.git"

func fixture() (Snapshot, domain.Application, map[string]any) {
	destination := map[string]any{"name": "test-cluster", "namespace": "demo"}
	request := map[string]any{
		"repo":          map[string]any{"repo": repository, "password": "fictional-repo-password"},
		"repos":         []any{map[string]any{"repo": "https://charts.example.com", "password": "fictional-dependency-password"}},
		"helmRepoCreds": []any{map[string]any{"url": "https://charts.example.com", "username": "reader"}},
		"kubeVersion":   "1.32.0", "apiVersions": []any{"v1", "apps/v1"},
		"appLabelKey": "app.kubernetes.io/instance", "trackingMethod": "annotation", "installationID": "test-installation",
		"helmOptions":        map[string]any{"valuesFileSchemes": []any{"https"}},
		"enabledSourceTypes": map[string]any{"Helm": true, "Plugin": false},
	}
	snapshot := Snapshot{Version: "v1alpha1", Instance: "test", ContextID: "fixture-v1", ArgoCDNamespace: "argocd", Bindings: []Binding{{
		Project: "demo", ApplicationNamespace: "argocd", Applications: []string{"new-child"}, RepositoryURL: repository,
		Destination: destination, Request: request,
	}}}
	app := domain.Application{ID: domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "new-child"}, Spec: map[string]any{
		"project": "demo", "destination": domain.CloneSpec(destination), "source": map[string]any{
			"repoURL": repository, "path": "charts/api", "targetRevision": "feature",
			"helm": map[string]any{
				"valueFiles": []any{"values.yaml", "values-test.yaml"}, "values": "replicas: 2\n",
				"valuesObject":   map[string]any{"replicas": json.Number("3"), "large": json.Number("9007199254740993"), "nested": map[string]any{"enabled": true}},
				"parameters":     []any{map[string]any{"name": "replicas", "value": "4", "forceString": true}},
				"fileParameters": []any{map[string]any{"name": "settings", "path": "settings.yaml"}},
				"releaseName":    "changed-release", "namespace": "helm-namespace", "kubeVersion": "1.33.0", "apiVersions": []any{"example.com/v1/Widget"},
				"version": "3", "passCredentials": false, "ignoreMissingValueFiles": true, "skipCrds": true, "skipTests": true, "skipSchemaValidation": true,
			},
		},
	}}
	project := map[string]any{"sourceRepos": []any{repository}, "destinations": []any{domain.CloneSpec(destination)}}
	return snapshot, app, project
}

func fakeAPI(t *testing.T, spec func(int) map[string]any) *argocd.Client {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/projects/demo" {
			t.Error("desired render attempted an Application lookup or mutation")
			http.NotFound(w, r)
			return
		}
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "demo"}, "spec": spec(calls)})
	}))
	t.Cleanup(server.Close)
	return &argocd.Client{Instance: "test", HTTP: httpx.New(server.URL, "fictional-api-token", time.Second, 1<<20)}
}

func issueCode(err error) string {
	var failure *domain.Failure
	if errors.As(err, &failure) {
		return failure.Issue.Code
	}
	return ""
}

func TestDesiredRenderPreservesHelmContext(t *testing.T) {
	for _, namespace := range []string{"argocd", "team-apps"} {
		t.Run(namespace, func(t *testing.T) {
			snapshot, app, project := fixture()
			app.ID.Namespace, snapshot.Bindings[0].ApplicationNamespace = namespace, namespace
			project["sourceNamespaces"] = []any{namespace}
			original := domain.CloneSpec(app.Spec)
			originalRequest := domain.CloneSpec(snapshot.Bindings[0].Request)
			calls := 0
			client := &Client{API: fakeAPI(t, func(int) map[string]any { return project }), snapshot: snapshot}
			client.invoke = func(_ context.Context, request map[string]any) (Response, error) {
				invokeMu.Lock()
				defer invokeMu.Unlock()
				calls++
				source := request["applicationSource"].(map[string]any)
				helm := source["helm"].(map[string]any)
				wireValues := helm["valuesObject"].(map[string]any)["raw"].(string)
				raw, err := base64.StdEncoding.DecodeString(wireValues)
				if err != nil {
					t.Fatal(err)
				}
				decoder := json.NewDecoder(strings.NewReader(string(raw)))
				decoder.UseNumber()
				var values map[string]any
				if err = decoder.Decode(&values); err != nil {
					t.Fatal(err)
				}
				helm["valuesObject"] = values
				source["targetRevision"] = "feature"
				if !reflect.DeepEqual(source, app.Source()) {
					t.Fatal("Helm inputs changed across the protobuf wire representation")
				}
				name := app.ID.Name
				if namespace != "argocd" {
					name = namespace + "_" + name
				}
				if request["revision"] != commit || request["appName"] != name || request["namespace"] != "demo" || request["projectName"] != "demo" || request["noCache"] != true || request["noRevisionCache"] != true || request["hasMultipleSources"] != false {
					t.Fatal("revision, identity or isolation context missing")
				}
				for key, value := range originalRequest {
					if !reflect.DeepEqual(request[key], value) {
						t.Fatalf("trusted context lost: %s", key)
					}
				}
				return Response{Revision: commit, SourceType: "Helm", Manifests: []string{"fixture"}}, nil
			}
			result, err := client.RenderDesired(context.Background(), app, commit)
			// Every Helm render is repeated to find fields that differ
			// between identical renders.
			if err != nil || len(result) != 1 || calls != 2 {
				t.Fatalf("new desired spec did not render twice: calls=%d %v", calls, err)
			}
			if !reflect.DeepEqual(original, app.Spec) || !reflect.DeepEqual(originalRequest, snapshot.Bindings[0].Request) {
				t.Fatal("render mutated frozen inputs")
			}
		})
	}
}

func TestDesiredRenderFailureBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name, code string
		change     func(*Snapshot, *domain.Application, map[string]any)
	}{
		{"instance", "routing_invalid", func(_ *Snapshot, a *domain.Application, _ map[string]any) { a.ID.Instance = "other" }},
		{"name", "access_denied", func(_ *Snapshot, a *domain.Application, _ map[string]any) { a.ID.Name = "not-authorized" }},
		{"destination", "access_denied", func(_ *Snapshot, a *domain.Application, _ map[string]any) {
			a.Spec["destination"].(map[string]any)["namespace"] = "other"
		}},
		{"repository", "access_denied", func(_ *Snapshot, a *domain.Application, _ map[string]any) {
			a.Source()["repoURL"] = "https://github.com/example/other.git"
		}},
		{"ambiguous binding", "configuration_invalid", func(s *Snapshot, _ *domain.Application, _ map[string]any) {
			s.Bindings = append(s.Bindings, s.Bindings[0])
		}},
		{"project repo denied", "access_denied", func(_ *Snapshot, _ *domain.Application, p map[string]any) { p["sourceRepos"] = []any{} }},
		{"project destination denied", "access_denied", func(_ *Snapshot, _ *domain.Application, p map[string]any) { p["destinations"] = []any{} }},
		{"invalid project pattern", "unsupported_project_policy", func(_ *Snapshot, _ *domain.Application, p map[string]any) { p["sourceRepos"] = []any{"!*"} }},
		{"project deny rule", "access_denied", func(_ *Snapshot, _ *domain.Application, p map[string]any) {
			p["sourceRepos"] = []any{"*", "!" + repository}
		}},
		{"scoped clusters", "unsupported_project_policy", func(_ *Snapshot, _ *domain.Application, p map[string]any) {
			p["permitOnlyProjectScopedClusters"] = true
		}},
		{"namespace denied", "access_denied", func(s *Snapshot, a *domain.Application, _ map[string]any) {
			a.ID.Namespace = "team"
			s.Bindings[0].ApplicationNamespace = "team"
		}},
		{"source integrity", "unsupported_project_policy", func(_ *Snapshot, _ *domain.Application, p map[string]any) { p["sourceIntegrity"] = map[string]any{} }},
		{"signature policy", "unsupported_project_policy", func(_ *Snapshot, _ *domain.Application, p map[string]any) {
			p["signatureKeys"] = []any{map[string]any{"keyID": "fixture"}}
		}},
		{"plugin", "unsupported_source", func(_ *Snapshot, a *domain.Application, _ map[string]any) {
			a.Source()["plugin"] = map[string]any{"name": "fixture"}
		}},
		// The backend applies preview's source rule with the same code.
		{"respected ignore rules", "unsupported_ignore_rules", func(_ *Snapshot, a *domain.Application, _ map[string]any) {
			a.Spec["ignoreDifferences"] = []any{map[string]any{"kind": "Deployment", "jsonPointers": []any{"/spec/replicas"}}}
			a.Spec["syncPolicy"] = map[string]any{"syncOptions": []any{"RespectIgnoreDifferences=true"}}
		}},
		{"invalid valuesObject", "unsupported_source", func(_ *Snapshot, a *domain.Application, _ map[string]any) {
			a.Source()["helm"].(map[string]any)["valuesObject"] = "not-an-object"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			snapshot, app, project := fixture()
			tt.change(&snapshot, &app, project)
			client := &Client{API: fakeAPI(t, func(int) map[string]any { return project }), snapshot: snapshot,
				invoke: func(context.Context, map[string]any) (Response, error) {
					t.Error("denied input reached repo-server")
					return Response{}, nil
				}}
			_, err := client.RenderDesired(context.Background(), app, commit)
			if issueCode(err) != tt.code {
				t.Fatalf("want %s, got %v", tt.code, err)
			}
		})
	}
}

func TestScopeBindingDiscoversNamesWithoutExpandingAccess(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		change     func(*Snapshot, *domain.Application, map[string]any)
	}{
		{"unregistered child", "", func(_ *Snapshot, a *domain.Application, _ map[string]any) { a.ID.Name = "payment-api" }},
		{"other instance", "routing_invalid", func(_ *Snapshot, a *domain.Application, _ map[string]any) { a.ID.Instance = "other" }},
		{"other project", "access_denied", func(_ *Snapshot, a *domain.Application, _ map[string]any) { a.Spec["project"] = "other" }},
		{"other repository", "access_denied", func(_ *Snapshot, a *domain.Application, _ map[string]any) {
			a.Source()["repoURL"] = "https://github.com/example/other.git"
		}},
		{"other namespace", "access_denied", func(_ *Snapshot, a *domain.Application, _ map[string]any) { a.ID.Namespace = "other" }},
		{"other cluster", "access_denied", func(_ *Snapshot, a *domain.Application, _ map[string]any) {
			a.Spec["destination"].(map[string]any)["name"] = "other"
		}},
		{"other destination namespace", "access_denied", func(_ *Snapshot, a *domain.Application, _ map[string]any) {
			a.Spec["destination"].(map[string]any)["namespace"] = "other"
		}},
		{"project disallows repo", "access_denied", func(_ *Snapshot, _ *domain.Application, p map[string]any) { p["sourceRepos"] = []any{} }},
		{"project disallows destination", "access_denied", func(_ *Snapshot, _ *domain.Application, p map[string]any) { p["destinations"] = []any{} }},
		{"invalid name", "configuration_invalid", func(_ *Snapshot, a *domain.Application, _ map[string]any) { a.ID.Name = "../invalid" }},
		{"overlapping scope and name", "configuration_invalid", func(s *Snapshot, _ *domain.Application, _ map[string]any) {
			b := s.Bindings[0]
			b.Applications = []string{"new-child"}
			s.Bindings = append(s.Bindings, b)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, app, project := fixture()
			snapshot.Bindings[0].Applications = nil
			tc.change(&snapshot, &app, project)
			calls := 0
			client := &Client{API: fakeAPI(t, func(int) map[string]any { return project }), snapshot: snapshot,
				invoke: func(_ context.Context, request map[string]any) (Response, error) {
					invokeMu.Lock()
					defer invokeMu.Unlock()
					calls++
					if request["appName"] != app.ID.Name {
						t.Fatal("new child identity lost")
					}
					return Response{Revision: commit, SourceType: "Helm", Manifests: []string{"rendered-child"}}, nil
				}}
			result, err := client.RenderDesired(context.Background(), app, commit)
			if issueCode(err) != tc.code {
				t.Fatalf("want %q, got %v", tc.code, err)
			}
			if tc.code == "" {
				// One render and its repeat.
				if calls != 2 || len(result) != 1 {
					t.Fatal("unregistered child was not rendered")
				}
			} else if calls != 0 {
				t.Fatal("out-of-scope request reached repo-server")
			}
		})
	}
}

func TestApplicationNameSelectorJSONContract(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		valid       bool
	}{
		{"omitted", "", true},
		{"null", `,"applications":null`, true},
		{"explicit names", `,"applications":["new-child"]`, true},
		{"empty list", `,"applications":[]`, false},
		{"wildcard name", `,"applications":["*"]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, _, _ := fixture()
			data, _ := json.Marshal(snapshot.Bindings[0])
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			delete(raw, "applications")
			data, _ = json.Marshal(raw)
			data = append(data[:len(data)-1], []byte(tc.field+"}")...)
			if err := json.Unmarshal(data, &snapshot.Bindings[0]); err != nil {
				t.Fatal(err)
			}
			// Decode into a fresh value so omitted fields cannot inherit old names.
			var binding Binding
			if err := json.Unmarshal(data, &binding); err != nil {
				t.Fatal(err)
			}
			snapshot.Bindings[0] = binding
			if err := snapshot.Validate("test"); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, got %v", tc.valid, err)
			}
		})
	}
}

func TestRevisionAndProjectDrift(t *testing.T) {
	for _, mode := range []string{"mutable", "response revision", "response type", "project drift"} {
		t.Run(mode, func(t *testing.T) {
			snapshot, app, project := fixture()
			client := &Client{API: fakeAPI(t, func(call int) map[string]any {
				copy := domain.CloneSpec(project)
				if mode == "project drift" && call > 1 {
					copy["description"] = "changed"
				}
				return copy
			}), snapshot: snapshot, invoke: func(context.Context, map[string]any) (Response, error) {
				if mode == "mutable" {
					t.Error("mutable revision reached RPC")
				}
				r := Response{Revision: commit, SourceType: "Helm"}
				if mode == "response revision" {
					r.Revision = strings.Repeat("b", 40)
				}
				if mode == "response type" {
					r.SourceType = "Plugin"
				}
				return r, nil
			}}
			revision, want := commit, "render_response_mismatch"
			if mode == "mutable" {
				revision, want = "main", "revision_invalid"
			}
			if mode == "project drift" {
				want = "render_context_changed"
			}
			_, err := client.RenderDesired(context.Background(), app, revision)
			if issueCode(err) != want {
				t.Fatalf("want %s, got %v", want, err)
			}
		})
	}
}

func TestSnapshotValidationAndLoad(t *testing.T) {
	for _, mode := range []string{"valid", "wrong instance", "missing capabilities", "credential mismatch", "caller source", "enabled plugin", "missing installation", "unknown field", "extra JSON"} {
		t.Run(mode, func(t *testing.T) {
			snapshot, _, _ := fixture()
			switch mode {
			case "wrong instance":
				snapshot.Instance = "other"
			case "missing capabilities":
				snapshot.Bindings[0].Request["apiVersions"] = []any{}
			case "credential mismatch":
				snapshot.Bindings[0].Request["repo"] = map[string]any{"repo": "https://other.example.com"}
			case "caller source":
				snapshot.Bindings[0].Request["applicationSource"] = map[string]any{}
			case "enabled plugin":
				snapshot.Bindings[0].Request["enabledSourceTypes"].(map[string]any)["Plugin"] = true
			case "missing installation":
				delete(snapshot.Bindings[0].Request, "installationID")
			}
			data, _ := json.Marshal(snapshot)
			if mode == "unknown field" {
				data = append([]byte(`{"unknown":true,`), data[1:]...)
			}
			if mode == "extra JSON" {
				data = append(data, []byte(` {}`)...)
			}
			file := filepath.Join(t.TempDir(), "context.json")
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			executable, _ := os.Executable()
			client, err := New(&argocd.Client{Instance: "test"}, profile.RepoServer{ContextFile: file, GRPCURL: executable}, 1<<20)
			if mode == "valid" {
				if err != nil || client.BackendInfo().ContextID != "fixture-v1" || client.BackendInfo().Verification != "live-unverified" {
					t.Fatal("snapshot provenance missing", err)
				}
			} else if issueCode(err) != "configuration_invalid" {
				t.Fatalf("invalid snapshot accepted: %v", err)
			}
		})
	}
}

// The child executable simulates grpcurl's process protocol, not Helm rendering.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == Method {
		if os.Getenv("DIFFARO_PROCESS_SECRET") != "" {
			os.Exit(90)
		}
		var importPath, protoName string
		for i := 1; i+1 < len(os.Args); i++ {
			switch os.Args[i] {
			case "-import-path":
				importPath = os.Args[i+1]
			case "-proto":
				protoName = os.Args[i+1]
			}
		}
		if importPath == "" || protoName == "" {
			os.Exit(92)
		}
		wire, err := os.ReadFile(filepath.Join(importPath, protoName))
		if err != nil || !strings.Contains(string(wire), "rpc GenerateManifest(ManifestRequest) returns (ManifestResponse)") {
			os.Exit(93)
		}
		var request map[string]any
		if json.NewDecoder(os.Stdin).Decode(&request) != nil {
			os.Exit(91)
		}
		switch request["testMode"] {
		case "tls":
			fmt.Fprintln(os.Stderr, "Failed to dial target: tls: first record does not look like a TLS handshake; Authorization: Bearer fictional-sensitive-diagnostic")
			os.Exit(1)
		case "certificate":
			fmt.Fprintln(os.Stderr, "x509: certificate signed by unknown authority; fictional-sensitive-diagnostic")
			os.Exit(1)
		case "unreachable":
			fmt.Fprintln(os.Stderr, "dial tcp: connection refused; fictional-sensitive-diagnostic")
			os.Exit(1)
		case "rpc-denied":
			fmt.Fprintln(os.Stderr, "ERROR: Code: PermissionDenied; fictional-sensitive-diagnostic")
			os.Exit(1)
		case "render-error":
			fmt.Fprintln(os.Stderr, "helm template failed --set password=fictional-sensitive-diagnostic")
			os.Exit(1)
		case "diagnostic-overflow":
			fmt.Fprint(os.Stderr, strings.Repeat("fictional-sensitive-diagnostic ", 4096))
			os.Exit(1)
		case "failure":
			fmt.Fprintln(os.Stderr, "fictional-secret-diagnostic")
			os.Exit(1)
		case "overflow":
			fmt.Print(strings.Repeat("x", 4096))
		case "invalid":
			fmt.Print("fictional-secret-invalid-json")
		case "extra":
			fmt.Print("{} {}")
		case "timeout":
			time.Sleep(10 * time.Second)
		default:
			_ = json.NewEncoder(os.Stdout).Encode(Response{Revision: commit, SourceType: "Helm", Manifests: []string{"fixture"}})
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestProcessIsolationAndFailures(t *testing.T) {
	t.Setenv("DIFFARO_PROCESS_SECRET", "fictional-inherited-token")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	invoke := processMethodInvoker(profile.RepoServer{GRPCURL: executable, Address: "127.0.0.1:8081", Plaintext: true}, 1024, Method, nil)
	for _, tt := range []struct{ mode, code string }{{"success", ""}, {"failure", "repo_server_failed"}, {"overflow", "limit_exceeded"}, {"invalid", "response_invalid"}, {"extra", "response_invalid"}, {"timeout", ""}} {
		t.Run(tt.mode, func(t *testing.T) {
			timeout := 5 * time.Second
			if tt.mode == "timeout" {
				timeout = 200 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			result, err := invoke(ctx, map[string]any{"testMode": tt.mode, "password": "fictional-request-secret"})
			if tt.mode == "timeout" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("process deadline was not preserved")
				}
				return
			}
			if issueCode(err) != tt.code {
				t.Fatalf("wrong process error: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "fictional") {
				t.Fatal("subprocess credential or diagnostic leaked")
			}
			if tt.mode == "success" && (err != nil || len(result.Manifests) != 1) {
				t.Fatal("process response missing", err)
			}
		})
	}
	_, err = invoke(context.Background(), map[string]any{"large": strings.Repeat("x", 1025)})
	if issueCode(err) != "limit_exceeded" {
		t.Fatal("request size was not bounded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = invoke(ctx, map[string]any{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
}
