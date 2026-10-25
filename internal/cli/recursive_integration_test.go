package cli

import (
	"bytes"
	"context"
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

func recursiveApp(name string) map[string]any {
	a := testApp()
	a["apiVersion"], a["kind"] = "argoproj.io/v1alpha1", "Application"
	a["metadata"].(map[string]any)["name"] = name
	s := a["spec"].(map[string]any)["source"].(map[string]any)
	s["path"], s["targetRevision"] = "charts/"+name, "stored-ref"
	return a
}

func recursiveServer(t *testing.T, scenario, token string, rootOnly, childOnly bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var childRenders atomic.Int32
	s := newArgoCDFake(t, argocdFake{token: token, apps: func(w http.ResponseWriter, r *http.Request) {
		name, manifestRequest := applicationPath(r.URL.Path)
		if (rootOnly && name != "api") || (childOnly && name != "child") || (name != "api" && name != "child") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("appNamespace") != "argocd" {
			t.Error("child Application namespace lost")
		}
		if scenario == "new" && name == "child" {
			http.NotFound(w, r)
			return
		}
		if scenario == "denied" && name == "child" {
			http.Error(w, "fictional-sensitive-error", 403)
			return
		}
		if !manifestRequest {
			writeJSON(w, recursiveApp(name))
			return
		}
		if r.URL.Query().Get("project") != "demo" {
			t.Error("project context lost")
		}
		rev := r.URL.Query().Get("revision")
		if rev != baseSHA && rev != headSHA {
			t.Error("nonimmutable graph revision")
		}
		manifests := []string{}
		if name == "api" {
			if !(scenario == "new" && rev == baseSHA) && !(scenario == "removed" && rev == headSHA) {
				child := recursiveApp("child")
				source := child["spec"].(map[string]any)["source"].(map[string]any)
				source["targetRevision"] = "parent-ref"
				if scenario == "modified" && rev == headSHA {
					source["helm"].(map[string]any)["releaseName"] = "changed-release"
				}
				raw, _ := json.Marshal(child)
				manifests = append(manifests, string(raw))
			}
		} else {
			childRenders.Add(1)
			replicas := 2
			if rev == headSHA {
				replicas = 3
			}
			manifests = append(manifests, fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api"},"spec":{"replicas":%d}}`, replicas))
		}
		writeManifests(w, manifests)
	}})
	return s, &childRenders
}

func recursiveConfig(t *testing.T, git string, endpoints ...string) string {
	t.Helper()
	file := configFile(t, git, endpoints...)
	target := "instance-0"
	if len(endpoints) == 2 {
		editProfile(t, file, "  - instance: instance-1\n    namespace: argocd\n    name: api\n", "")
		target = "instance-1"
	}
	appendProfile(t, file, "routing:\n  clusters:\n    - cluster: test-cluster\n      namespace: argocd\n      instance: "+target+"\n")
	return file
}

func TestCLIRecursiveStoredSpecCapability(t *testing.T) {
	for _, scenario := range []string{"normal", "new", "modified", "removed", "denied"} {
		t.Run(scenario, func(t *testing.T) {
			g := gitServer(t)
			a, calls := recursiveServer(t, scenario, "fictional-argo-0", false, false)
			report, code := runJSON(t, recursiveConfig(t, g.URL, a.URL), "--base", "main", "--head", "feature", "--recursive")
			var child domain.ApplicationResult
			for _, app := range report.Applications {
				if app.ID.Name == "child" {
					child = app
				}
			}
			if len(report.Applications) != 2 || report.Graph == nil {
				t.Fatal("recursive CLI did not discover child")
			}
			switch scenario {
			case "normal":
				if code != 1 || calls.Load() != 2 || child.State != "changed" || child.Execution != "complete" {
					t.Fatal("matching child spec did not render both target SHAs")
				}
			case "new":
				if code != 1 || calls.Load() != 0 || child.State != "added" || child.Execution != "skipped" || child.SkipReason == "" || len(child.Issues) != 0 || len(child.Resources) != 0 || child.HeadTarget.Execution != "skipped" || child.HeadTarget.SkipReason == "" || report.Graph.HeadComplete {
					t.Fatal("new child was silently rendered using unrelated stored spec")
				}
			case "modified":
				if code != 1 || calls.Load() != 1 || child.State != "changed" || child.Execution != "skipped" || child.SkipReason == "" || len(child.Issues) != 0 || len(child.Resources) != 0 || child.BaseTarget.Execution != "complete" || child.HeadTarget.Execution != "skipped" || len(child.SpecChanges) == 0 {
					t.Fatal("modified desired inputs were silently replaced")
				}
			case "removed":
				if code != 1 || calls.Load() != 1 || child.State != "removed" || child.Pruning == nil || child.Pruning.Certainty != "conditional" {
					t.Fatal("removed child target or pruning uncertainty missing")
				}
			case "denied":
				if code != 3 || calls.Load() != 0 || child.Execution != "denied" {
					t.Fatal("denied child hid parent result or was misclassified")
				}
			}
			if scenario == "new" || scenario == "modified" {
				for _, format := range []string{"markdown", "pr-markdown", "json"} {
					output, err := reporter.Encode(report, format, security.Redactor{})
					reason := child.SkipReason
					if format == "pr-markdown" {
						// Compact evidence is bounded; it must retain the concrete
						// cause while full Markdown and JSON retain the entire reason.
						reason = "not_found (HTTP 404)"
						if scenario == "modified" {
							reason = "/source/helm/releaseName"
						}
					}
					if err != nil || !strings.Contains(string(output), reason) || !strings.Contains(string(output), "incomplete") {
						t.Fatalf("%s omitted the analysis boundary: %v", format, err)
					}
				}
			}
		})
	}
}

func TestDoctorBasicAnalysisShowsSkippedChild(t *testing.T) {
	g := gitServer(t)
	a, calls := recursiveServer(t, "new", "fictional-argo-0", false, false)
	var output, diagnostic bytes.Buffer
	code := Run(context.Background(), []string{"doctor", "--config", recursiveConfig(t, g.URL, a.URL), "--render", "--recursive", "--base", "main", "--head", "feature", "--format", "json", "--quiet"}, &output, &diagnostic)
	var report domain.Report
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("doctor report missing: %v, %s", err, diagnostic.String())
	}
	for _, app := range report.Applications {
		if app.ID.Name == "child" {
			if code != 1 || calls.Load() != 0 || app.Execution != "skipped" || app.SkipReason == "" || report.Graph.HeadComplete || len(report.Issues) != 0 {
				t.Fatal("preflight concealed skipped analysis or required a detailed backend")
			}
			return
		}
	}
	t.Fatal("doctor did not report the observed new child")
}

func TestCLIRecursiveCrossInstance(t *testing.T) {
	g := gitServer(t)
	root, _ := recursiveServer(t, "normal", "fictional-argo-0", true, false)
	child, calls := recursiveServer(t, "normal", "fictional-argo-1", false, true)
	report, code := runJSON(t, recursiveConfig(t, g.URL, root.URL, child.URL), "--base", "main", "--head", "feature", "--recursive")
	if code != 1 || calls.Load() != 2 || len(report.Applications) != 2 || report.Graph.HeadEdges[0].Child.Instance != "instance-1" {
		t.Fatal("cross-instance child not rendered in its own backend")
	}
	for _, app := range report.Applications {
		if app.ID.Instance == "" || app.ID.Name == "" || app.Cluster == "" || app.Namespace == "" || app.HeadTarget == nil || len(app.HeadTarget.GraphPath) == 0 {
			t.Fatalf("JSON lost an Application's scope or graph path: %+v", app)
		}
	}
	for _, format := range []string{"json", "markdown"} {
		actual, err := reporter.Encode(report, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		// Human formats name the graph path and each Application's scope;
		// JSON carries the same facts as structured fields, checked below.
		if format == "json" {
			continue
		}
		facts := []string{report.Graph.HeadEdges[0].Parent.String(), report.Graph.HeadEdges[0].Child.String()}
		for _, app := range report.Applications {
			facts = append(facts, app.ID.String(), app.Cluster, app.Project, app.Namespace)
		}
		for _, fact := range facts {
			if fact != "" && !strings.Contains(string(actual), fact) {
				t.Fatalf("%s does not state %q", format, fact)
			}
		}
	}
}
