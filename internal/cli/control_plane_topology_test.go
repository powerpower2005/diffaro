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
	"strings"
	"sync/atomic"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

// Installation workloads belong to root, while the regional farms are separate
// discovery roots. Root + two central + four service clusters total seven.
// Repeated farm/api/worker names and in-cluster aliases are deliberate.
func topologyNames(instance string) []string {
	if instance == "root" {
		return []string{"central", "install-east", "install-west", "central-tool"}
	}
	return []string{"farms", "farm", "api", "worker"}
}

func topologyChildren(instance, name string) []string {
	if instance == "root" && name == "central" {
		return topologyNames(instance)[1:]
	}
	if name == "farms" {
		return []string{"farm"}
	}
	if name == "farm" {
		return topologyNames(instance)[2:]
	}
	return nil
}

func topologyApp(instance, name string) map[string]any {
	app := recursiveApp(name)
	spec := app["spec"].(map[string]any)
	cluster, namespace := "in-cluster", "argocd"
	if len(topologyChildren(instance, name)) == 0 {
		namespace = "demo"
		switch name {
		case "install-east", "install-west":
			cluster, namespace = strings.Replace(name, "install-", "central-", 1), "argocd"
		case "central-tool":
			cluster = "central-west" // Still managed by root, despite its destination.
		default:
			cluster = "service-" + instance
			if name == "worker" {
				cluster += "-secondary"
			}
		}
		chart := spec["source"].(map[string]any)
		chart["repoURL"], chart["targetRevision"] = libraryRepo, ancestorSHA
		chart["helm"].(map[string]any)["valueFiles"] = []any{"values.yaml", "$values/" + instance + "/" + name + ".yaml"}
		spec["sources"] = []any{chart, map[string]any{"repoURL": fakeRepo, "targetRevision": "main", "ref": "values"}}
		delete(spec, "source")
	}
	spec["destination"] = map[string]any{"name": cluster, "namespace": namespace}
	return app
}

func topologyServer(t *testing.T, instance, scenario, token string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := newArgoCDFake(t, argocdFake{token: token, before: func(http.ResponseWriter, *http.Request) bool {
		requests.Add(1)
		return false
	}, apps: func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/applications":
			if instance == "west" && scenario == "west-denied" {
				http.Error(w, "fictional-sensitive-error", http.StatusForbidden)
				return
			}
			items := []any{}
			if instance != "west" || scenario != "west-empty" {
				for _, name := range topologyNames(instance) {
					items = append(items, topologyApp(instance, name))
				}
			}
			writeJSON(w, map[string]any{"items": items})
			return
		}
		name, render := applicationPath(r.URL.Path)
		known := false
		for _, allowed := range topologyNames(instance) {
			known = known || name == allowed
		}
		if !known || r.URL.Query().Get("appNamespace") != "argocd" {
			t.Errorf("unexpected topology lookup: %s", r.URL.String())
			http.NotFound(w, r)
			return
		}
		if !render {
			writeJSON(w, topologyApp(instance, name))
			return
		}
		query := r.URL.Query()
		revision := query.Get("revision")
		children := topologyChildren(instance, name)
		if len(children) == 0 {
			if revision != "" || strings.Join(query["sourcePositions"], ",") != "1,2" || len(query["revisions"]) != 2 || query["revisions"][0] != ancestorSHA {
				t.Error("chart/values source positions or frozen external chart revision lost")
				http.Error(w, "invalid revision mapping", http.StatusBadRequest)
				return
			}
			revision = query["revisions"][1]
		}
		if (revision != ancestorSHA && revision != headSHA) || query.Get("project") != "demo" {
			t.Error("render did not use immutable PR merge-base/head and project")
		}
		manifests := []string{}
		for _, childName := range children {
			if instance == "east" && childName == "api" && revision == headSHA && scenario == "east-removed" {
				continue
			}
			child := topologyApp(instance, childName)
			if instance == "east" && childName == "api" && revision == headSHA && scenario == "east-modified" {
				source := child["spec"].(map[string]any)["sources"].([]any)[0].(map[string]any)
				source["helm"].(map[string]any)["releaseName"] = "changed-release"
			}
			raw, _ := json.Marshal(child)
			manifests = append(manifests, string(raw))
		}
		if len(children) == 0 {
			replicas := 2
			if revision == headSHA {
				replicas = map[string]int{"root": 3, "east": 4, "west": 5}[instance]
				if name == "worker" {
					replicas += 10 // Distinguish destinations within the same instance too.
				}
			}
			// Equal resource names across destinations must remain separate.
			manifests = append(manifests, fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api"},"spec":{"replicas":%d}}`, replicas))
		}
		writeManifests(w, manifests)
	}})
	return server, &requests
}

func TestControlPlaneTopology(t *testing.T) {
	for _, scenario := range []string{"normal", "west-denied", "east-modified", "east-removed", "west-empty"} {
		t.Run(scenario, func(t *testing.T) {
			git := gitServer(t)
			t.Setenv("DIFFARO_TEST_GITHUB", "fictional-github-token")
			var catalog strings.Builder
			catalog.WriteString("instances:\n")
			counters := []*atomic.Int32{}
			for _, instance := range []string{"root", "east", "west"} {
				key, token := "DIFFARO_TOPOLOGY_"+strings.ToUpper(instance), "fictional-topology-"+instance
				t.Setenv(key, token)
				server, counter := topologyServer(t, instance, scenario, token)
				counters = append(counters, counter)
				fmt.Fprintf(&catalog, "  - id: %s\n    endpoint: %s\n    tokenEnv: %s\n", instance, server.URL, key)
			}
			dir := t.TempDir()
			catalogPath, config := filepath.Join(dir, "connections.yaml"), filepath.Join(dir, "profile.yaml")
			if err := os.WriteFile(catalogPath, []byte(catalog.String()), 0600); err != nil {
				t.Fatal(err)
			}
			pr := "https://github.com/example/charts/pull/12"
			var out, diagnostic bytes.Buffer
			if code := Run(context.Background(), []string{"init", "--connections", catalogPath, "--local-children", "--pr", pr, "--git-api", git.URL, "--git-token-env", "DIFFARO_TEST_GITHUB", "--config", config}, &out, &diagnostic); code != 0 {
				t.Fatalf("topology setup: exit=%d %s", code, diagnostic.String())
			}
			for _, counter := range counters {
				if counter.Load() != 0 {
					t.Fatal("catalog init contacted a backend")
				}
			}
			parsed, err := profile.Load(config)
			if err != nil || len(parsed.Applications) != 0 || len(parsed.Routing.Clusters) != 6 || parsed.Routing.DefaultInstance != "" {
				t.Fatal("setup requires individual roots/children or a global default")
			}
			if scenario == "normal" {
				out.Reset()
				diagnostic.Reset()
				if code := Run(context.Background(), []string{"doctor", "--config", config, "--render", "--pr", pr, "--quiet"}, &out, &diagnostic); code != 1 {
					t.Fatalf("topology doctor: exit=%d %s", code, diagnostic.String())
				}
			}
			report, code := runJSON(t, config, "--pr", pr) // Discovery is recursive without --recursive.
			wantExit := 1
			if scenario == "west-denied" {
				wantExit = 3
			}
			if code != wantExit || report.Graph == nil || report.Revisions.Base != ancestorSHA || report.Revisions.Head != headSHA {
				t.Fatalf("topology result: exit=%d issues=%+v", code, report.Issues)
			}
			for _, name := range []string{"install-east", "install-west", "central-tool"} {
				app := multiInstanceResult(t, report, "root", name)
				if app.Execution != "complete" || app.Root.Instance != "root" || len(app.GraphPath) != 2 || app.Cluster == "in-cluster" {
					t.Fatal("central workload destination was mistaken for its managing instance")
				}
			}
			worker := multiInstanceResult(t, report, "east", "worker")
			if worker.Execution != "complete" || worker.Cluster != "service-east-secondary" || len(worker.GraphPath) != 3 {
				t.Fatal("independent east workload lost its recursive path or target")
			}
			for _, edge := range append(append([]domain.GraphEdge{}, report.Graph.BaseEdges...), report.Graph.HeadEdges...) {
				if edge.Parent.Instance != edge.Child.Instance || strings.HasPrefix(edge.Parent.Name, "install-") {
					t.Fatal("installation relationship invented a management edge")
				}
			}
			for _, app := range report.Applications {
				for _, resource := range app.Resources {
					if resource.ID.Cluster != app.ID.Instance+":"+app.Cluster {
						t.Fatal("target identity crossed its managing instance/destination")
					}
				}
			}
			switch scenario {
			case "normal":
				if len(report.Applications) != 12 || !report.Graph.BaseComplete || !report.Graph.HeadComplete {
					t.Fatal("three-instance graph collapsed repeated Application names or lost coverage")
				}
				// in-cluster is local to each controller, not one global cluster.
				physicalClusters := map[string]bool{}
				for _, app := range report.Applications {
					cluster := app.Cluster
					if cluster == "in-cluster" {
						cluster = map[string]string{"root": "root-cluster", "east": "central-east", "west": "central-west"}[app.ID.Instance]
					}
					physicalClusters[cluster] = true
				}
				wantClusters := []string{"root-cluster", "central-east", "central-west", "service-east", "service-east-secondary", "service-west", "service-west-secondary"}
				if len(physicalClusters) != len(wantClusters) {
					t.Fatalf("expected seven physical destinations, got %v", physicalClusters)
				}
				for _, cluster := range wantClusters {
					if !physicalClusters[cluster] {
						t.Fatalf("missing tested cluster %s", cluster)
					}
				}
				for _, instance := range []string{"east", "west"} {
					for _, name := range []string{"api", "worker"} {
						app := multiInstanceResult(t, report, instance, name)
						cluster, replicas := "service-"+instance, map[string]int{"east": 4, "west": 5}[instance]
						if name == "worker" {
							cluster, replicas = cluster+"-secondary", replicas+10
						}
						if app.Execution != "complete" || app.Cluster != cluster || len(app.GraphPath) != 3 || len(app.Resources) != 1 || len(app.Resources[0].Changes) != 1 || app.Resources[0].Changes[0].Path != "/spec/replicas" || app.Resources[0].Changes[0].Before != float64(2) || app.Resources[0].Changes[0].After != float64(replicas) {
							t.Fatalf("regional target comparison mixed instance or destination: %s/%s", instance, name)
						}
						for _, side := range []struct {
							target   *domain.TargetSummary
							revision string
						}{{app.BaseTarget, ancestorSHA}, {app.HeadTarget, headSHA}} {
							if side.target == nil || side.target.Execution != "complete" || side.target.Cluster != cluster || len(side.target.Sources) != 2 || side.target.Sources[0].Revision != ancestorSHA || side.target.Sources[1].Revision != side.revision {
								t.Fatal("external chart and PR values provenance lost in a service cluster")
							}
						}
					}
				}
			case "west-denied":
				if report.Status != "partial_failure" || report.Graph.BaseComplete || report.Graph.HeadComplete || multiInstanceResult(t, report, "east", "api").Execution != "complete" {
					t.Fatal("west denial hid root/east results or claimed complete analysis")
				}
			case "east-modified":
				app := multiInstanceResult(t, report, "east", "api")
				if app.Execution != "skipped" || len(app.SpecChanges) == 0 || app.HeadTarget.Execution != "skipped" || len(app.Resources) != 0 || multiInstanceResult(t, report, "west", "api").Execution != "complete" {
					t.Fatal("modified child was treated as unchanged or damaged independent west analysis")
				}
			case "east-removed":
				app := multiInstanceResult(t, report, "east", "api")
				if app.State != "removed" || app.Pruning == nil || app.Pruning.Certainty != "conditional" || len(app.Pruning.Resources) != 1 || multiInstanceResult(t, report, "west", "api").State != "changed" {
					t.Fatal("removed east child lost pruning evidence or removed its west namesake")
				}
			case "west-empty":
				if len(report.Applications) != 8 || !report.Graph.BaseComplete || !report.Graph.HeadComplete {
					t.Fatal("empty west inventory fabricated Applications or failed independent analysis")
				}
				for _, selection := range report.Discovery {
					if selection.Instance == "west" && selection.Status != "no_matches" {
						t.Fatal("empty inventory did not report its discovery boundary")
					}
				}
			}
			out.Reset()
			diagnostic.Reset()
			mdPath, jsonPath := filepath.Join(dir, "preview.md"), filepath.Join(dir, "preview.json")
			if artifactExit := Run(context.Background(), []string{"diff", "--config", config, "--pr", pr, "--quiet", "--format", "pr-markdown", "--output", mdPath, "--json-output", jsonPath}, &out, &diagnostic); artifactExit != wantExit {
				t.Fatalf("topology artifacts: exit=%d %s", artifactExit, diagnostic.String())
			}
			md, mdErr := os.ReadFile(mdPath)
			machine, jsonErr := os.ReadFile(jsonPath)
			var saved domain.Report
			if mdErr != nil || jsonErr != nil || json.Unmarshal(machine, &saved) != nil || saved.Revisions != report.Revisions || len(saved.Applications) != len(report.Applications) || !bytes.Contains(md, []byte(headSHA)) || !bytes.Contains(md, []byte("<!-- kubeseer:preview -->")) {
				t.Fatal("paired artifacts lost PR identity or independent results")
			}
			firstJSON, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			secondJSON, err := json.Marshal(saved)
			if err != nil || !bytes.Equal(firstJSON, secondJSON) {
				t.Fatal("repeated topology comparison changed the machine report")
			}
			assertNoSecrets(t, "topology report", string(md)+string(machine)+diagnostic.String(), "fictional-")
		})
	}
}
