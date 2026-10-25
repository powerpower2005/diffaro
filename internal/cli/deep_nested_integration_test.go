package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

// This graph is also used by the disposable, real-Argo Helm acceptance run.
// HTTP fixtures validate orchestration; they do not substitute for Helm proof.
type nestedFixture struct {
	Roots []string            `json:"roots"`
	Base  map[string][]string `json:"base"`
	Added map[string][]string `json:"added"`
}

func readNestedFixture(t *testing.T) nestedFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/deep-nested/graph.json")
	if err != nil {
		t.Fatal(err)
	}
	var f nestedFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func nestedGraph(f nestedFixture, head bool) map[string][]string {
	out := map[string][]string{}
	for name, children := range f.Base {
		out[name] = append([]string{}, children...)
	}
	for name, children := range f.Added {
		out[name] = append([]string{}, children...)
	}
	if head {
		out["org-a"] = []string{"region-a", "new-parent"}
	}
	return out
}

func nestedApplication(name string, graph map[string][]string) map[string]any {
	namespace := "argocd"
	if len(graph[name]) == 0 {
		namespace = "demo"
	}
	return map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"namespace": "argocd", "name": name},
		"spec": map[string]any{
			"project":     "demo",
			"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": namespace},
			"source": map[string]any{
				"repoURL": fakeRepo, "path": "nodes/" + name, "targetRevision": "HEAD",
				"helm": map[string]any{"releaseName": name},
			},
		},
	}
}

func nestedBackend(t *testing.T, f nestedFixture, fault string) *httptest.Server {
	t.Helper()
	all := nestedGraph(f, false)
	s := newArgoCDFake(t, argocdFake{token: "fictional-argo-0", apps: func(w http.ResponseWriter, r *http.Request) {
		name, render := applicationPath(r.URL.Path)
		if _, exists := all[name]; !exists || r.URL.Query().Get("appNamespace") != "argocd" {
			http.NotFound(w, r)
			return
		}
		if !render {
			writeJSON(w, nestedApplication(name, all))
			return
		}
		rev := r.URL.Query().Get("revision")
		if (rev != baseSHA && rev != headSHA) || r.URL.Query().Get("project") != "demo" {
			t.Error("nested render lost immutable revision or project")
		}
		head := rev == headSHA
		if head && fault == "broken" && name == "region-b" {
			http.Error(w, "fictional-sensitive-error", http.StatusInternalServerError)
			return
		}
		graph := nestedGraph(f, head)
		children := append([]string{}, graph[name]...)
		if head && fault == "cycle" && name == "rollout-shared" {
			children = append(children, "org-a")
		}
		manifests := []string{}
		for _, child := range children {
			app := nestedApplication(child, all)
			if head && fault == "conflict" && name == "env-b" && child == "a-shared" {
				app["spec"].(map[string]any)["source"].(map[string]any)["helm"].(map[string]any)["releaseName"] = "conflicting-shared"
			}
			data, _ := json.Marshal(app)
			manifests = append(manifests, string(data))
		}
		if len(all[name]) == 0 {
			replicas, unavailable, message, selector := 3, 0, "before", name
			if head && name == "workload-a" {
				replicas, unavailable, message, selector = 1, 1, "after", "missing"
			}
			manifests = append(manifests,
				fmt.Sprintf(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":%q,"namespace":"demo"},"data":{"message":%q}}`, name+"-config", message),
				fmt.Sprintf(`{"apiVersion":"v1","kind":"Service","metadata":{"name":%q,"namespace":"demo"},"spec":{"ports":[{"port":80,"targetPort":8080}],"selector":{"app":%q}}}`, name, selector),
				fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":%q,"namespace":"demo"},"spec":{"replicas":%d,"strategy":{"type":"RollingUpdate","rollingUpdate":{"maxUnavailable":%d,"maxSurge":1}},"selector":{"matchLabels":{"app":%q}},"template":{"metadata":{"labels":{"app":%q}},"spec":{"containers":[{"name":"api","image":"example.invalid/fixture/api:0.1.0","envFrom":[{"configMapRef":{"name":%q}}]}]}}}}`, name, replicas, unavailable, name, name, name+"-config"),
			)
		}
		writeManifests(w, manifests)
	}})
	return s
}

func TestDeepNestedAppOfApps(t *testing.T) {
	f := readNestedFixture(t)
	for _, scenario := range []string{"lifecycle", "reverse", "unchanged", "policy", "broken", "cycle", "conflict", "depth-limit", "application-limit"} {
		t.Run(scenario, func(t *testing.T) {
			g := gitServer(t)
			a := nestedBackend(t, f, scenario)
			cfg := configFile(t, g.URL, a.URL)
			editProfile(t, cfg, "name: api", "name: nested-root-a")
			text := "  - instance: instance-0\n    namespace: argocd\n    name: nested-root-b\nrouting:\n  defaultInstance: instance-0\n"
			if scenario == "depth-limit" {
				text += "render:\n  maxDepth: 4\n"
			}
			if scenario == "application-limit" {
				text += "render:\n  maxApplications: 12\n"
			}
			if scenario == "policy" {
				text += "policies:\n  - id: retain-deployments\n    rule: deny-resource-removal-v1\n    match:\n      group: apps\n      kind: Deployment\n"
			}
			appendProfile(t, cfg, text)
			base, head, want := "main", "feature", 1
			if scenario == "reverse" {
				base, head = head, base
			}
			if scenario == "unchanged" {
				base, want = head, 0
			}
			if scenario == "policy" {
				want = 2
			}
			if scenario == "broken" || scenario == "cycle" || scenario == "conflict" || strings.HasSuffix(scenario, "limit") {
				want = 3
			}
			r, code := runJSON(t, cfg, "--base", base, "--head", head, "--recursive")
			if code != want || r.Graph == nil {
				t.Fatalf("nested graph exit=%d expected=%d", code, want)
			}
			apps := map[string]domain.ApplicationResult{}
			for _, app := range r.Applications {
				if _, duplicate := apps[app.ID.Name]; duplicate {
					t.Fatal("shared Application duplicated")
				}
				apps[app.ID.Name] = app
			}
			if want < 3 {
				count := 30
				if scenario == "unchanged" {
					count = 25
				}
				if len(apps) != count || !r.Graph.BaseComplete || !r.Graph.HeadComplete || len(r.Graph.BaseEdges) != 25 || len(r.Graph.HeadEdges) != 25 {
					t.Fatal("deep graph omitted nodes or edges")
				}
				incoming := 0
				for _, edge := range r.Graph.HeadEdges {
					if edge.Child.Name == "a-shared" {
						incoming++
					}
				}
				if incoming != 3 || apps["workload-b"].State != "unchanged" || apps["workload-shared"].State != "unchanged" {
					t.Fatal("shared subtree edges or independent unchanged targets lost")
				}
				if len(apps["workload-a"].GraphPath) != 8 || len(apps["workload-shared"].GraphPath) != 5 {
					t.Fatal("deep path or shared shortcut provenance lost")
				}
				if scenario == "unchanged" {
					for _, app := range apps {
						if app.State != "unchanged" || app.Execution != "complete" || r.ChangesFound {
							t.Fatal("same-revision graph invented changes or failures")
						}
					}
				} else {
					from, to := float64(3), float64(1)
					if scenario == "reverse" {
						from, to = to, from
					}
					replicaChange := false
					for _, resource := range apps["workload-a"].Resources {
						for _, change := range resource.Changes {
							if change.Path == "/spec/replicas" && change.Before == from && change.After == to {
								replicaChange = true
							}
						}
					}
					if !replicaChange {
						t.Fatal("deep workload changes lost")
					}
				}
				if scenario != "unchanged" {
					removed, added := "retired-", "new-"
					if scenario == "reverse" {
						removed, added = added, removed
					}
					for name, app := range apps {
						if strings.HasPrefix(name, removed) && (app.State != "removed" || app.Pruning == nil || app.Pruning.Certainty != "conditional") {
							t.Fatal("removed descendant lost conditional pruning evidence")
						}
						if strings.HasPrefix(name, added) && (app.State != "added" || app.Execution != "complete") {
							t.Fatal("added subtree was not rendered")
						}
					}
				}
				if scenario == "policy" && (r.Policy == nil || r.Policy.Evaluation != "complete" || len(r.Policy.Violations) != 1 || r.Policy.Violations[0].Application.Name != "retired-workload") {
					t.Fatal("nested leaf removal policy lost evidence")
				}
			} else {
				if r.Status != "partial_failure" || r.Graph.HeadComplete {
					t.Fatal("incomplete graph advertised complete results")
				}
				issues := append([]domain.Issue{}, r.Issues...)
				for _, app := range apps {
					issues = append(issues, app.Issues...)
				}
				issueCode := map[string]string{"broken": "backend_error", "cycle": "graph_cycle", "conflict": "graph_spec_conflict", "depth-limit": "graph_limit_exceeded", "application-limit": "graph_limit_exceeded"}[scenario]
				foundIssue := false
				for _, issue := range issues {
					if issue.Code == issueCode {
						foundIssue = true
					}
				}
				if !foundIssue {
					t.Fatalf("nested failure omitted actionable %s", issueCode)
				}
				if scenario == "broken" || scenario == "cycle" || scenario == "conflict" {
					if apps["workload-a"].Execution != "complete" || apps["retired-workload"].State != "unknown" || apps["retired-workload"].Pruning != nil || apps["new-workload"].State != "added" {
						t.Fatal("partial graph lost independent evidence or falsely proved removal")
					}
				}
				if scenario == "broken" && (apps["workload-b"].State != "unknown" || apps["workload-shared"].Execution != "complete") {
					t.Fatal("failed middle parent hid shared results or falsely removed an undiscovered leaf")
				}
				if scenario == "cycle" && apps["workload-b"].Execution != "complete" {
					t.Fatal("cyclic branch hid independent root results")
				}
				if scenario == "application-limit" {
					baseNodes, headNodes := 0, 0
					for _, app := range apps {
						if app.BaseTarget != nil {
							baseNodes++
						}
						if app.HeadTarget != nil {
							headNodes++
						}
					}
					if baseNodes > 12 || headNodes > 12 {
						t.Fatal("unique Application limit was exceeded")
					}
				}
				if scenario == "conflict" {
					for _, name := range []string{"a-shared", "service-shared", "rollout-shared", "workload-shared"} {
						if apps[name].Execution == "complete" {
							t.Fatal("conflicting ancestor left trusted descendant evidence")
						}
						for _, resource := range apps[name].Resources {
							if resource.Comparison == "complete" {
								t.Fatal("conflicting ancestor left a trusted resource comparison")
							}
						}
					}
				}
				if scenario == "depth-limit" {
					sharedDepthFault := false
					for _, issue := range r.Issues {
						if issue.Code == "graph_limit_exceeded" && strings.Contains(issue.Reason, "shared subtree") {
							sharedDepthFault = true
						}
					}
					if !sharedDepthFault {
						t.Fatal("short shared path bypassed the longest-path depth bound")
					}
				}
			}
			// Exercise both artifact formats over the same graph, not only JSON.
			if scenario == "lifecycle" {
				for _, format := range []string{"markdown", "pr-markdown"} {
					md := filepath.Join(t.TempDir(), "preview.md")
					var stdout, stderr bytes.Buffer
					code := Run(context.Background(), []string{"diff", "--config", cfg, "--base", base, "--head", head, "--recursive", "--format", format, "--output", md, "--json-output", md + ".json", "--quiet"}, &stdout, &stderr)
					data, err := os.ReadFile(md + ".json")
					var repeated domain.Report
					if code != 1 || stdout.Len() != 0 || stderr.Len() != 0 || err != nil || json.Unmarshal(data, &repeated) != nil || !reflect.DeepEqual(r, repeated) {
						t.Fatal("same-run artifacts changed the nested comparison")
					}
					markdown, err := os.ReadFile(md)
					if err != nil || (format == "pr-markdown" && (len(markdown) > 60*1024 || !bytes.Contains(markdown, []byte("<!-- kubeseer:preview -->")))) {
						t.Fatal("nested Markdown artifact lost its bounds")
					}
					assertNoSecrets(t, "nested Markdown artifact", string(markdown), "fictional-")
				}
			}
		})
	}
}
