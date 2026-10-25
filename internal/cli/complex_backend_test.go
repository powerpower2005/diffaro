package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/reposerver"
)

func complexApp(name string, instance int, revision string) map[string]any {
	baseName := name
	if name == "old" || name == "new" {
		baseName = "east"
	}
	a := multiRepositoryApp(baseName, instance, "multi-repo")
	a["metadata"].(map[string]any)["name"] = name
	spec := a["spec"].(map[string]any)
	if sources, ok := spec["sources"].([]any); ok {
		source := sources[0].(map[string]any)
		source["path"] = "charts/" + name
		if name == "east" && revision == headSHA {
			source["helm"].(map[string]any)["releaseName"] = "east-updated"
		}
	}
	return a
}

func complexContext(instance int, cluster, repository string) map[string]any {
	request := mockRequestContext()
	request["repo"] = map[string]any{"repo": repository, "password": fmt.Sprintf("fictional-repo-%d", instance)}
	request["installationID"] = fmt.Sprintf("fixture-%d", instance)
	request["kubeVersion"] = fmt.Sprintf("1.%d.0", 30+instance)
	request["apiVersions"] = []any{"v1", "apps/v1", "argoproj.io/v1alpha1", cluster + ".example.com/v1/Widget"}
	return request
}

// Real stdin/subprocess path for detailed rendering, including refSources and
// instance-local credentials. All manifests are deterministic fake RPC output.
// complexRPC is the fake grpcurl for complexConfig instance 0 or 1 (roles in
// main_test.go); failEastHead fails the head render of Application east.
func complexRPC(instance int, failEastHead bool) error {
	for _, variable := range os.Environ() {
		if strings.HasPrefix(variable, "DIFFARO_") {
			return fmt.Errorf("API credential environment inherited by RPC process")
		}
	}
	if strings.Contains(strings.Join(os.Args, " "), "fictional-") {
		return fmt.Errorf("RPC credential in argv")
	}
	var request map[string]any
	decoder := json.NewDecoder(os.Stdin)
	decoder.UseNumber()
	if err := decoder.Decode(&request); err != nil {
		return err
	}
	name := domain.String(request, "appName")
	if name != "api" && name != "middle" && name != "east" && name != "west" && name != "old" && name != "new" {
		return fmt.Errorf("unexpected Application identity")
	}
	source := request["applicationSource"].(map[string]any)
	repository := domain.String(source, "repoURL")
	cluster, namespace := "in-cluster", "argocd"
	if name != "api" && name != "middle" {
		cluster, namespace = "east", "demo"
	}
	if name == "west" {
		cluster = "west"
	}
	for key, expected := range complexContext(instance, cluster, repository) {
		if !reflect.DeepEqual(request[key], expected) {
			return fmt.Errorf("wrong instance or cluster render context: %s", key)
		}
	}
	if request["namespace"] != namespace || request["noCache"] != true || request["noRevisionCache"] != true {
		return fmt.Errorf("render identity/cache isolation missing")
	}
	revision := domain.String(request, "revision")
	pairRevision := revision
	if request["hasMultipleSources"] == true {
		refs := request["refSources"].(map[string]any)
		values := refs["$values"].(map[string]any)
		shared := refs["$shared"].(map[string]any)
		pairRevision = domain.String(values, "targetRevision")
		if len(refs) != 2 || domain.String(values["repo"].(map[string]any), "repo") != fakeRepo || domain.String(shared["repo"].(map[string]any), "repo") != sharedValuesRepo || domain.String(shared, "targetRevision") != sharedValuesSHA {
			return fmt.Errorf("external repository inputs or revisions crossed sources")
		}
		for _, ref := range []map[string]any{values, shared} {
			if domain.String(ref["repo"].(map[string]any), "password") != fmt.Sprintf("fictional-repo-%d", instance) {
				return fmt.Errorf("external repository credential crossed instance")
			}
		}
		if repository == libraryRepo && revision != ancestorSHA || repository == sharedValuesRepo && revision != sharedValuesSHA || repository == fakeRepo && revision != pairRevision {
			return fmt.Errorf("PR SHA substituted for external repository SHA")
		}
	}
	if pairRevision != baseSHA && pairRevision != headSHA {
		return fmt.Errorf("comparison revision missing")
	}
	if failEastHead && name == "east" && pairRevision == headSHA {
		return fmt.Errorf("fictional-sensitive-error")
	}
	manifests := []string{}
	if domain.String(source, "path") != "" {
		switch name {
		case "api", "middle":
			children := []string{"middle"}
			if name == "middle" {
				removedOrAdded := "old"
				if pairRevision == headSHA {
					removedOrAdded = "new"
				}
				children = []string{"east", "west", removedOrAdded}
			}
			for _, child := range children {
				raw, _ := json.Marshal(complexApp(child, instance, pairRevision))
				manifests = append(manifests, string(raw))
			}
		default:
			replicas := 2
			if pairRevision == headSHA {
				replicas = 3 + instance
			}
			manifests = []string{fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":%q},"spec":{"replicas":%d}}`, name, replicas)}
			if name == "east" && pairRevision == headSHA && domain.String(source["helm"].(map[string]any), "releaseName") != "east-updated" {
				return fmt.Errorf("changed desired Helm inputs replaced with stored inputs")
			}
		}
	}
	sourceType := "Helm"
	if domain.String(source, "path") == "" {
		sourceType = ""
	}
	return json.NewEncoder(os.Stdout).Encode(reposerver.Response{Revision: revision, SourceType: sourceType, Manifests: manifests})
}

func complexConfig(t *testing.T, mode string) string {
	t.Helper()
	git := gitServer(t)
	endpoints := []string{}
	for instance := 0; instance < 2; instance++ {
		server := readOnlyFake(t, "Argo CD", fmt.Sprintf("fictional-argo-%d", instance), func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/version":
				fmt.Fprint(w, `{"Version":"v3.5.3"}`)
			case "/api/v1/applications/api":
				_ = json.NewEncoder(w).Encode(complexApp("api", instance, baseSHA))
			case "/api/v1/projects/demo":
				_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "demo"}, "spec": map[string]any{"sourceRepos": []any{fakeRepo, libraryRepo, sharedValuesRepo}, "destinations": []any{map[string]any{"name": "in-cluster", "namespace": "argocd"}, map[string]any{"name": "east", "namespace": "demo"}, map[string]any{"name": "west", "namespace": "demo"}}}})
			default:
				t.Errorf("detailed backend attempted stored child lookup or another API: %s", r.URL.Path)
				http.NotFound(w, r)
			}
		})
		endpoints = append(endpoints, server.URL)
	}
	file := multiInstanceConfig(t, git.URL, endpoints[0], endpoints[1], "normal")
	for instance := 0; instance < 2; instance++ {
		snapshot := reposerver.Snapshot{Version: "v1alpha1", Instance: fmt.Sprintf("instance-%d", instance), ContextID: fmt.Sprintf("complex-%d", instance), ArgoCDNamespace: "argocd"}
		for _, cluster := range []string{"in-cluster", "east", "west"} {
			namespace := "demo"
			repositories := []string{libraryRepo, fakeRepo, sharedValuesRepo}
			if cluster == "in-cluster" {
				namespace, repositories = "argocd", []string{fakeRepo}
			}
			for _, repository := range repositories {
				binding := reposerver.Binding{Project: "demo", ApplicationNamespace: "argocd", RepositoryURL: repository, Destination: map[string]any{"name": cluster, "namespace": namespace}, Request: complexContext(instance, cluster, repository)}
				if mode == "values-denied" && instance == 0 && cluster == "east" && repository == fakeRepo {
					binding.Applications = []string{"old", "new"}
				}
				snapshot.Bindings = append(snapshot.Bindings, binding)
			}
		}
		raw, _ := json.Marshal(snapshot)
		contextFile := fmt.Sprintf("complex-%d.json", instance)
		if err := os.WriteFile(filepath.Join(filepath.Dir(file), contextFile), raw, 0600); err != nil {
			t.Fatal(err)
		}
		role, port := []string{rpcComplex0, rpcComplex1}[instance], 19100+instance
		if instance == 0 && mode == "render-failed" {
			role, port = rpcComplex0EastHeadFails, 19102
		}
		quoted, _ := json.Marshal(fakeGRPCURL(t, role))
		key := fmt.Sprintf("    tokenEnv: DIFFARO_TEST_ARGO_%d", instance)
		block := fmt.Sprintf("%s\n    repoServer:\n      address: 127.0.0.1:%d\n      plaintext: true\n      grpcurl: %s\n      contextFile: %s", key, port, quoted, contextFile)
		editProfile(t, file, key, block)
	}
	return file
}

func TestDetailedRecursiveMultipleInstancesRepositoriesAndClusters(t *testing.T) {
	for _, mode := range []string{"normal", "values-denied", "render-failed"} {
		t.Run(mode, func(t *testing.T) {
			report, code := runJSON(t, complexConfig(t, mode), "--base", "main", "--head", "feature", "--recursive")
			want := 1
			if mode != "normal" {
				want = 3
			}
			if code != want || len(report.Applications) != 12 {
				t.Fatalf("complex detailed comparison failed: exit=%d apps=%d issues=%+v", code, len(report.Applications), report.Issues)
			}
			for _, name := range []string{"east", "west", "new", "old"} {
				app := multiInstanceResult(t, report, "instance-1", name)
				if app.Execution != "complete" || len(app.Resources) != 1 {
					t.Fatalf("independent detailed target lost or marked failed: %s state=%s execution=%s resources=%d issues=%+v", name, app.State, app.Execution, len(app.Resources), app.Issues)
				}
				if name == "new" && app.State != "added" || name == "old" && (app.State != "removed" || app.Pruning == nil) || name == "east" && (app.State != "changed" || len(app.SpecChanges) == 0) {
					t.Fatal("new/removed/modified desired child was replaced or skipped")
				}
			}
			if mode == "values-denied" {
				app := multiInstanceResult(t, report, "instance-0", "east")
				if app.Execution != "denied" || app.Issues[0].SourcePosition != 2 || app.Issues[0].Repository != fakeRepo {
					t.Fatal("external values permission failure lost source-specific evidence")
				}
			}
			if mode == "render-failed" && multiInstanceResult(t, report, "instance-0", "east").Execution != "failed" {
				t.Fatal("detailed RPC failure downgraded to skip")
			}
			if mode == "normal" && (!report.Graph.BaseComplete || !report.Graph.HeadComplete) {
				t.Fatal("detailed target graph unexpectedly incomplete")
			}
			raw, _ := json.Marshal(report)
			assertNoSecrets(t, "report with subprocess credentials or raw RPC error", string(raw), "fictional-")
		})
	}
}
