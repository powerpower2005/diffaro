package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/reporter"
	"diffaro/internal/security"
)

// Both instances deliberately use the same Application names and cluster aliases.
// Each root discovers an intermediate Application and two workload clusters.
func multiInstanceApp(name string) map[string]any {
	a := recursiveApp(name)
	cluster, namespace := "in-cluster", "argocd"
	if name == "east" || name == "west" {
		cluster, namespace = name, "demo"
	}
	a["spec"].(map[string]any)["destination"] = map[string]any{"name": cluster, "namespace": namespace}
	return a
}

func multiInstanceServer(t *testing.T, instance int, scenario string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var leafRenders atomic.Int32
	s := newArgoCDFake(t, argocdFake{token: fmt.Sprintf("fictional-argo-%d", instance), apps: func(w http.ResponseWriter, r *http.Request) {
		name, isRender := applicationPath(r.URL.Path)
		if name != "api" && name != "middle" && name != "east" && name != "west" {
			t.Errorf("unexpected backend request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("appNamespace") != "argocd" {
			t.Error("Application namespace lost")
		}
		if instance == 0 && ((scenario == "root-denied" && name == "api") || ((scenario == "child-denied" || scenario == "removed-403") && name == "east")) {
			http.Error(w, "fictional-sensitive-error", http.StatusForbidden)
			return
		}
		if !isRender {
			writeJSON(w, multiRepositoryApp(name, instance, scenario))
			return
		}
		rev := r.URL.Query().Get("revision")
		if strings.HasPrefix(scenario, "multi-repo") && (name == "east" || name == "west") {
			q := r.URL.Query()
			if rev != "" || strings.Join(q["sourcePositions"], ",") != "1,2,3" || len(q["revisions"]) != 3 || q["revisions"][0] != ancestorSHA || q["revisions"][2] != sharedValuesSHA {
				t.Error("multi-repository plan lost source order or reused PR revisions for external repositories")
				http.Error(w, "bad source plan", http.StatusBadRequest)
				return
			}
			rev = q["revisions"][1]
		}
		if (rev != baseSHA && rev != headSHA && rev != ancestorSHA) || r.URL.Query().Get("project") != "demo" {
			t.Error("render lost immutable revision or project")
		}
		var manifests []string
		switch name {
		case "api", "middle":
			children := []string{"middle"}
			if name == "middle" {
				children = []string{"east", "west"}
			}
			for _, childName := range children {
				if instance == 0 && childName == "east" && rev == headSHA && (scenario == "removed" || scenario == "removed-403") {
					continue
				}
				child := multiRepositoryApp(childName, instance, scenario)
				if instance == 0 && childName == "east" && rev == headSHA && scenario == "modified" {
					child["spec"].(map[string]any)["source"].(map[string]any)["helm"].(map[string]any)["releaseName"] = "changed-release"
				}
				if instance == 0 && childName == "east" && rev == headSHA && scenario == "cluster-moved" {
					child["spec"].(map[string]any)["destination"].(map[string]any)["name"] = "west"
				}
				raw, _ := json.Marshal(child)
				manifests = append(manifests, string(raw))
			}
		default:
			leafRenders.Add(1)
			if instance == 0 && name == "east" && rev == headSHA && scenario == "multi-repo-render-failed" {
				http.Error(w, "fictional-sensitive-error", http.StatusInternalServerError)
				return
			}
			if instance == 0 && name == "east" && rev == headSHA && scenario == "render-failed" {
				http.Error(w, "fictional-sensitive-error", http.StatusInternalServerError)
				return
			}
			label, selector, replicas := "west-only", "west-only", 2
			if instance == 0 && name == "east" {
				label, selector = "api", "api"
			}
			if rev == headSHA {
				replicas = 3 + instance
				if instance == 0 && name == "east" {
					selector = "west-only"
				}
			}
			manifests = []string{
				fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api"},"spec":{"replicas":%d,"template":{"metadata":{"labels":{"app":%q}},"spec":{"containers":[{"name":"api","image":"example.invalid/api:v1"}]}}}}`, replicas, label),
				fmt.Sprintf(`{"apiVersion":"v1","kind":"Service","metadata":{"name":"api"},"spec":{"selector":{"app":%q},"ports":[{"port":80}]}}`, selector),
			}
			if instance == 0 && name == "east" && scenario == "masked-secret" {
				manifests = append(manifests, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials"},"data":{"token":"++++++++"}}`)
			}
		}
		writeManifests(w, manifests)
	}})
	return s, &leafRenders
}

func multiInstanceConfig(t *testing.T, git, first, second, scenario string) string {
	t.Helper()
	file := configFile(t, git, first, second)
	var routes strings.Builder
	routes.WriteString("routing:\n  clusters:\n")
	for i := 0; i < 2; i++ {
		if i == 0 && scenario == "routing-missing" {
			continue
		}
		fmt.Fprintf(&routes, "    - parentInstance: instance-%d\n      cluster: in-cluster\n      namespace: argocd\n      instance: instance-%d\n", i, i)
	}
	if scenario == "routing-ambiguous" {
		// A legacy global selector conflicts only with instance-0's scoped rule.
		routes.WriteString("    - cluster: in-cluster\n      namespace: argocd\n      instance: instance-1\n")
	}
	appendProfile(t, file, routes.String())
	return file
}

func multiInstanceResult(t *testing.T, report domain.Report, instance, name string) domain.ApplicationResult {
	t.Helper()
	for _, app := range report.Applications {
		if app.ID.Instance == instance && app.ID.Namespace == "argocd" && app.ID.Name == name {
			return app
		}
	}
	t.Fatalf("missing %s/argocd/%s", instance, name)
	return domain.ApplicationResult{}
}

func TestCLIMultipleInstancesAndClusters(t *testing.T) {
	for _, tc := range []struct {
		scenario string
		exit     int
	}{
		{"normal", 1}, {"child-denied", 3}, {"root-denied", 3}, {"modified", 1},
		{"removed", 1}, {"removed-403", 3}, {"render-failed", 3}, {"routing-missing", 3}, {"routing-ambiguous", 3}, {"masked-secret", 1}, {"cluster-moved", 1},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			git := gitServer(t)
			first, _ := multiInstanceServer(t, 0, tc.scenario)
			second, calls := multiInstanceServer(t, 1, tc.scenario)
			config := multiInstanceConfig(t, git.URL, first.URL, second.URL, tc.scenario)
			report, code := runJSON(t, config, "--base", "main", "--head", "feature", "--recursive")
			if code != tc.exit || report.Graph == nil || report.Revisions.Base != baseSHA || report.Revisions.Head != headSHA {
				t.Fatalf("exit=%d status=%s issues=%+v", code, report.Status, report.Issues)
			}
			if calls.Load() != 4 {
				t.Fatal("independent instance did not render both revisions in both clusters")
			}
			for _, name := range []string{"east", "west"} {
				app := multiInstanceResult(t, report, "instance-1", name)
				if app.Execution != "complete" || app.State != "changed" || app.Cluster != name || len(app.Resources) != 2 || len(app.GraphPath) != 3 || app.HeadTarget.Revision != headSHA {
					t.Fatalf("independent result lost identity, provenance or comparison: %+v", app)
				}
				for _, resource := range app.Resources {
					if resource.ID.Cluster != "instance-1:"+name || resource.ID.Namespace != "demo" {
						t.Fatal("target resources crossed cluster or instance boundaries")
					}
					if resource.ID.Kind == "Deployment" && (len(resource.Changes) != 1 || resource.Changes[0].Path != "/spec/replicas" || resource.Changes[0].After != float64(4)) {
						t.Fatal("instance-specific target output was replaced by another backend's result")
					}
				}
			}
			switch tc.scenario {
			case "normal":
				if len(report.Applications) != 8 || !report.Graph.BaseComplete || !report.Graph.HeadComplete {
					t.Fatal("duplicate names collapsed or graph discovery incomplete")
				}
			case "modified":
				app := multiInstanceResult(t, report, "instance-0", "east")
				if app.State != "changed" || app.Execution != "skipped" || app.BaseTarget.Execution != "complete" || app.HeadTarget.Execution != "skipped" || len(app.Resources) != 0 || len(app.SpecChanges) == 0 {
					t.Fatal("modified child used stored inputs or hid incomplete analysis")
				}
			case "cluster-moved":
				app := multiInstanceResult(t, report, "instance-0", "east")
				if app.State != "changed" || app.Execution != "skipped" || app.BaseTarget.Cluster != "east" || app.HeadTarget.Cluster != "west" || len(app.Resources) != 0 {
					t.Fatal("workload cluster move was hidden or invented target loss from unrendered resources")
				}
			case "removed":
				app := multiInstanceResult(t, report, "instance-0", "east")
				if app.State != "removed" || app.Pruning == nil || app.Pruning.Certainty != "conditional" || app.HeadTarget != nil {
					t.Fatal("removed child lost target pruning evidence")
				}
			case "child-denied", "removed-403":
				if multiInstanceResult(t, report, "instance-0", "east").Execution != "denied" {
					t.Fatal("403 was treated as absence or a harmless skip")
				}
			case "root-denied":
				if multiInstanceResult(t, report, "instance-0", "api").Execution != "denied" {
					t.Fatal("denied root was silently dropped")
				}
			case "render-failed":
				if multiInstanceResult(t, report, "instance-0", "east").Execution != "failed" {
					t.Fatal("backend failure downgraded to skip")
				}
			case "masked-secret":
				app := multiInstanceResult(t, report, "instance-0", "east")
				unknown := false
				for _, res := range app.Resources {
					unknown = unknown || (res.ID.Kind == "Secret" && res.State == "unknown")
				}
				if app.Execution != "complete" || len(app.Issues) != 0 || !unknown {
					t.Fatal("API-masked Secret equality was invented, or it failed the Application")
				}
			case "routing-missing", "routing-ambiguous":
				want := strings.ReplaceAll(tc.scenario, "-", "_")
				found := false
				for _, issue := range report.Issues {
					found = found || issue.Code == want
				}
				if !found {
					t.Fatalf("routing failure lost: %+v", report.Issues)
				}
			}
			if tc.exit == 3 && (report.Status != "partial_failure") {
				t.Fatal("partial run advertised complete analysis")
			}
			for _, format := range []string{"json", "markdown", "pr-markdown"} {
				output, err := reporter.Encode(report, format, security.Redactor{})
				if err != nil {
					t.Fatal(format, err)
				}
				assertNoSecrets(t, format+" with credentials, inline values, backend errors or rendered secrets", string(output), "fictional-")
				if tc.scenario == "normal" {
					// Repeat the real CLI flow, including requests, to protect stable reports.
					again, againCode := runJSON(t, config, "--base", "main", "--head", "feature", "--recursive")
					repeated, err := reporter.Encode(again, format, security.Redactor{})
					if err != nil || againCode != code || string(output) != string(repeated) {
						t.Fatalf("%s report is not reproducible", format)
					}
				}
			}
		})
	}
}
