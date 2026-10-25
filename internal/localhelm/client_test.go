package localhelm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
	"diffaro/internal/httpx"
	"diffaro/internal/preview"
	"diffaro/internal/profile"
	"diffaro/internal/security"
)

func init() {
	if len(os.Args) == 2 && os.Args[1] == "__helm-render" {
		if os.Getenv("DIFFARO_LOCAL_TEST_SECRET") != "" || os.Getenv("KUBECONFIG") != "" {
			os.Exit(6)
		}
		os.Exit(Worker(os.Stdin, os.Stdout))
	}
}

func TestBundledWorkerIsolationAndCancellation(t *testing.T) {
	t.Setenv("DIFFARO_LOCAL_TEST_SECRET", "fictional-secret")
	t.Setenv("KUBECONFIG", "must-not-be-read")
	req := fixtureRequest()
	want, err := Render(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Process(context.Background(), req)
	if err != nil || !reflect.DeepEqual(want, got) {
		t.Fatal("worker did not preserve inputs or isolate credentials", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = Process(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatal("worker ignored cancellation", err)
	}
	// A stopped render names its step for the timeout issue.
	expired, stop := context.WithTimeout(context.Background(), 0)
	defer stop()
	_, err = Process(expired, req)
	var failure *domain.Failure
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &failure) || failure.Issue.Code != domain.CodeTimeout || failure.Issue.Stage != "render" || failure.Issue.Reason != "bundled Helm render did not finish" {
		t.Fatal("stopped render lost its step", err)
	}
}

func TestWorkerFailurePreservesActualSource(t *testing.T) {
	req := fixtureRequest()
	repository := "https://github.com/example/other"
	source := map[string]any{"repoURL": repository, "path": "broken", "targetRevision": testSHA, "helm": map[string]any{}}
	req.Plan = append(req.Plan, domain.SourceRevision{Position: 2, Repository: repository, Revision: testSHA, Source: source})
	req.Repositories[repository+"@"+testSHA] = map[string][]byte{"broken/Chart.yaml": []byte("apiVersion: v2\nname: broken\nversion: 0.1.0\n"), "broken/values.yaml": []byte("secret: fictional-private-value\n"), "broken/templates/error.yaml": []byte(`{{ fail .Values.secret }}`)}
	out, err := Process(context.Background(), req)
	var failure *domain.Failure
	if len(out) != 0 || !errors.As(err, &failure) || failure.Issue.SourcePosition != 2 || failure.Issue.Repository != repository || failure.Issue.Revision != testSHA || strings.Contains(err.Error(), "fictional-private-value") {
		t.Fatal("worker lost the failing source or exposed raw values", err)
	}
}

type fixtureGit struct {
	files map[string]map[string][]byte
	links map[string][]string
}

func (g fixtureGit) Archive(_ context.Context, repo, sha string) (map[string][]byte, []string, error) {
	files := g.files[repo+"@"+sha]
	if files == nil {
		return nil, nil, renderError()
	}
	return files, g.links[repo+"@"+sha], nil
}
func (g fixtureGit) Resolve(context.Context, string, string) (string, error) { return testSHA, nil }

// This uses the real Helm engine, three authenticated mock Argo APIs and seven
// distinct destinations. No target manifests are supplied by the API mocks.
func TestLocalHelmRecursiveDesiredChildren(t *testing.T) {
	headSHA := strings.Repeat("b", 40)
	makeFiles := func(changed bool) map[string][]byte {
		files := map[string][]byte{}
		for _, chart := range []string{"central", "farms", "farm", "workload", "install"} {
			files[chart+"/Chart.yaml"] = []byte("apiVersion: v2\nname: " + chart + "\nversion: 0.1.0\n")
			files[chart+"/values.yaml"] = []byte(fmt.Sprintf("region: east\nchanged: %t\nreplicas: 2\n", changed))
		}
		files["central/templates/apps.yaml"] = []byte(`{{- range $name := list "east" "west" }}
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: install-{{ $name }}, namespace: argocd}
spec:
  project: demo
  source:
    repoURL: https://github.com/example/charts
    path: install
    targetRevision: main
    helm: {releaseName: install-{{ $name }}}
  destination: {name: central-{{ $name }}, namespace: argocd}
{{- end }}
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: central-tool, namespace: argocd}
spec:
  project: demo
  source:
    repoURL: https://github.com/example/charts
    path: workload
    targetRevision: main
    helm: {releaseName: central-tool}
  destination: {name: central-west, namespace: demo}
`)
		files["farms/templates/apps.yaml"] = []byte(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: farm, namespace: argocd}
spec:
  project: demo
  source:
    repoURL: https://github.com/example/charts
    path: farm
    targetRevision: main
    helm:
      parameters: [{name: region, value: {{ .Values.region | quote }}}]
  destination: {name: in-cluster, namespace: argocd}
`)
		files["farm/templates/apps.yaml"] = []byte(`{{- $names := list "api" "worker" }}
{{- if and .Values.changed (eq .Values.region "east") }}{{ $names = append $names "new-api" }}{{ end }}
{{- range $name := $names }}
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: {{ $name }}, namespace: argocd}
spec:
  project: demo
  source:
    repoURL: https://github.com/example/charts
    path: workload
    targetRevision: main
    helm:
      releaseName: {{ $name }}
      {{- if and $.Values.changed (eq $.Values.region "east") (eq $name "api") }}
      valuesObject: {replicas: 7}
      {{- end }}
  destination:
    name: service-{{ $.Values.region }}{{ if ne $name "api" }}-secondary{{ end }}
    namespace: demo
{{- end }}
`)
		for _, chart := range []string{"workload", "install"} {
			files[chart+"/templates/deployment.yaml"] = []byte(`apiVersion: apps/v1
kind: Deployment
metadata: {name: {{ .Release.Name }}}
spec:
  replicas: {{ if .Values.changed }}{{ .Values.replicas }}{{ else }}2{{ end }}
  selector: {matchLabels: {app: {{ .Release.Name }}}}
  template:
    metadata: {labels: {app: {{ .Release.Name }}}}
    spec:
      containers: [{name: api, image: example.invalid/api:1}]
`)
		}
		if changed {
			files["workload/values.yaml"] = []byte("changed: true\nreplicas: 1\n")
		}
		return files
	}
	for _, scenario := range []string{"complete", "west-denied", "east-context-missing", "external-complete", "external-west-denied", "external-east-context-missing", "partial-workload", "external-partial-workload", "partial-root", "external-partial-root"} {
		t.Run(scenario, func(t *testing.T) {
			externalMode := strings.HasPrefix(scenario, "external-")
			scenario = strings.TrimPrefix(scenario, "external-")
			if externalMode && os.Getenv("DIFFARO_TEST_HELM_BINARY") == "" {
				t.Skip("set DIFFARO_TEST_HELM_BINARY for real external CLI graph tests")
			}
			git := fixtureGit{files: map[string]map[string][]byte{"example/charts@" + testSHA: makeFiles(false), "example/charts@" + headSHA: makeFiles(true)}}
			if strings.HasPrefix(scenario, "partial-") {
				chart := "workload"
				if scenario == "partial-root" {
					chart = "central"
				}
				for _, files := range git.files {
					files[chart+"/templates/allowlist.yaml"] = []byte(`{{ mustExampleIPs "demo" }}`)
				}
			}
			config := profile.Config{Render: profile.DefaultRender(), Git: profile.Git{RepositoryURL: testRepo}, Routing: profile.Routing{}}
			renderers := map[string]domain.Renderer{}
			for _, id := range []string{"root", "east", "west"} {
				id := id
				rootName, rootPath := "farms", "farms"
				if id == "root" {
					rootName, rootPath = "central", "central"
				}
				app := domain.Application{ID: domain.ApplicationID{Instance: id, Namespace: "argocd", Name: rootName}, Spec: map[string]any{"project": "demo", "destination": map[string]any{"name": "in-cluster", "namespace": "argocd"}, "source": map[string]any{"repoURL": testRepo, "path": rootPath, "targetRevision": "main", "helm": map[string]any{"parameters": []any{map[string]any{"name": "region", "value": id}}}}}}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer token-"+id {
						t.Error("mutation or crossed instance token")
						w.WriteHeader(403)
						return
					}
					if id == "west" && scenario == "west-denied" {
						w.WriteHeader(403)
						return
					}
					switch {
					case r.URL.Path == "/api/v1/applications/"+rootName:
						json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": rootName, "namespace": "argocd"}, "spec": app.Spec})
					case r.URL.Path == "/api/v1/projects/demo":
						json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "demo"}, "spec": map[string]any{"sourceRepos": []any{"*"}, "destinations": []any{map[string]any{"name": "*", "namespace": "*"}}}})
					case r.URL.Path == "/api/v1/settings":
						json.NewEncoder(w).Encode(argocd.RenderSettings{AppLabelKey: "app.kubernetes.io/instance", TrackingMethod: "annotation", ControllerNamespace: "argocd", InstallationID: id})
					case strings.HasPrefix(r.URL.Path, "/api/v1/clusters/"):
						name := strings.TrimPrefix(r.URL.Path, "/api/v1/clusters/")
						versions := []string{"v1", "apps/v1/Deployment"}
						if id == "east" && scenario == "east-context-missing" && name == "service-east" {
							versions = nil
						}
						json.NewEncoder(w).Encode(map[string]any{"server": "https://" + id + "-" + name + ".example.invalid", "name": name, "info": map[string]any{"serverVersion": "1.31.0", "apiVersions": versions, "connectionState": map[string]any{"status": "Successful"}}})
					default:
						t.Error("local renderer attempted installed-child or repo-server lookup: " + r.URL.Path)
						w.WriteHeader(404)
					}
				}))
				defer server.Close()
				client := &Client{API: &argocd.Client{Instance: id, HTTP: httpx.New(server.URL, "token-"+id, time.Second, 1<<20)}, Git: git, RepositoryURL: testRepo, Limit: 1 << 20, Execute: func(_ context.Context, req Request) ([]string, error) { return Render(req) }}
				if externalMode {
					e, err := NewExternal(context.Background(), os.Getenv("DIFFARO_TEST_HELM_BINARY"))
					if err != nil {
						t.Fatal(err)
					}
					defer e.Close()
					client.Execute, client.External = nil, e
				}
				renderers[id] = client
				config.Applications = append(config.Applications, app.ID)
				config.Instances = append(config.Instances, profile.Instance{ID: id, LocalHelm: true})
				config.Routing.Clusters = append(config.Routing.Clusters, profile.ClusterRoute{ParentInstance: id, Cluster: "in-cluster", Namespace: "argocd", Instance: id})
			}
			runner := preview.Runner{Config: config, Renderers: renderers, Redactor: security.Redactor{Secrets: []string{"token-root", "token-east", "token-west"}}}
			pair := domain.RevisionPair{Repository: testRepo, Base: testSHA, Head: headSHA, Mode: "pr-merge-base-head"}
			report := runner.RunRecursive(context.Background(), pair)
			if scenario == "west-denied" || strings.HasPrefix(scenario, "partial-") {
				if report.Status != "partial_failure" || report.ExitCode() != 3 {
					t.Fatalf("%s: %+v", report.Status, report.Issues)
				}
			} else if report.Status != "success" || report.ExitCode() != 1 {
				t.Fatalf("%s: %+v", report.Status, report.Issues)
			}
			if strings.HasPrefix(scenario, "partial-") {
				found := false
				for _, app := range report.Applications {
					if app.Execution == "failed" && len(app.Resources) > 0 && len(app.Issues) == 2 && app.Issues[0].Code == "render_partial" {
						found = true
					}
				}
				if !found || report.Graph.BaseComplete || report.Graph.HeadComplete {
					t.Fatal("partial client targets lost or overstated coverage", report)
				}
			}
			if scenario == "complete" {
				if len(report.Applications) != 13 || !report.Graph.BaseComplete || !report.Graph.HeadComplete {
					t.Fatal("incomplete desired graph", report.Issues)
				}
				destinations := map[string]bool{}
				foundNew, foundChanged := false, false
				for _, app := range report.Applications {
					cluster := app.Cluster
					if cluster == "in-cluster" {
						cluster = map[string]string{"root": "root-cluster", "east": "central-east", "west": "central-west"}[app.ID.Instance]
					}
					destinations[cluster] = true
					if app.ID.Instance == "east" && app.ID.Name == "new-api" {
						foundNew = app.State == "added" && app.Execution == "complete" && len(app.Resources) == 1
					}
					if app.ID.Instance == "east" && app.ID.Name == "api" {
						for _, resource := range app.Resources {
							for _, change := range resource.Changes {
								if change.Path == "/spec/replicas" && fmt.Sprint(change.Before) == "2" && fmt.Sprint(change.After) == "7" {
									foundChanged = len(app.SpecChanges) > 0 && app.Execution == "complete"
								}
							}
						}
					}
				}
				if len(destinations) != 7 || !foundNew || !foundChanged {
					t.Fatalf("destinations=%v new=%t changed=%t", destinations, foundNew, foundChanged)
				}
			}
			if scenario == "east-context-missing" {
				found := false
				for _, app := range report.Applications {
					if app.ID.Instance == "east" && app.ID.Name == "api" {
						found = app.Execution == "skipped" && len(app.Resources) == 0
					}
				}
				if !found {
					t.Fatal("missing capabilities became an empty/unchanged target")
				}
			}
			for _, edge := range append(report.Graph.BaseEdges, report.Graph.HeadEdges...) {
				if edge.Parent.Instance != edge.Child.Instance || strings.HasPrefix(edge.Parent.Name, "install-") {
					t.Fatal("installation was mistaken for Application management")
				}
			}
			machine, _ := json.Marshal(report)
			if strings.Contains(string(machine), "token-") {
				t.Fatal("credential leak")
			}
		})
	}
}

func TestWorkerPreservesPartialTargets(t *testing.T) {
	req := fixtureRequest()
	req.Repositories[testRepo+"@"+testSHA]["app/templates/broken.yaml"] = []byte(`{{ mustExampleIPs "fictional-private-value" }}`)
	out, err := Process(context.Background(), req)
	var failure *domain.Failure
	if len(out) != 1 || !domain.IsRenderPartial(err) || !errors.As(err, &failure) || failure.Issue.SourcePosition != 1 || failure.Issue.Repository != testRepo || failure.Issue.Revision != testSHA || !strings.Contains(failure.Issue.Action, "custom Helm") || strings.Contains(err.Error(), "fictional-private-value") {
		t.Fatalf("worker dropped partial targets/context: %v %v", out, err)
	}
}

func TestWorkerPreservesEveryPartialSource(t *testing.T) {
	req := fixtureRequest()
	req.Repositories[testRepo+"@"+testSHA]["app/templates/broken.yaml"] = []byte(`{{ mustExampleIPs "demo" }}`)
	repo := "https://github.com/example/second"
	source := map[string]any{"repoURL": repo, "path": "other", "targetRevision": testSHA, "helm": map[string]any{}}
	req.Plan = append(req.Plan, domain.SourceRevision{Position: 2, Repository: repo, Revision: testSHA, Source: source})
	req.Repositories[repo+"@"+testSHA] = map[string][]byte{
		"other/Chart.yaml":            []byte("apiVersion: v2\nname: other\nversion: 0.1.0\n"),
		"other/templates/broken.yaml": []byte(`{{ mustOtherIPs "demo" }}`),
		"other/templates/config.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: other}\n"),
	}
	out, err := Process(context.Background(), req)
	issues := domain.RenderIssues(err)
	if len(out) != 2 || !domain.IsRenderPartial(err) || len(issues) != 2 || issues[0].Repository != testRepo || issues[0].SourcePosition != 1 || issues[1].Repository != repo || issues[1].SourcePosition != 2 || !strings.Contains(issues[1].Reason, "mustOtherIPs") {
		t.Fatalf("lost multi-source partial context: %v %v %+v", out, err, issues)
	}
}
func TestPartialClientStillRejectsDrift(t *testing.T) {
	for _, stage := range []string{"application", "project"} {
		t.Run(stage, func(t *testing.T) {
			req := fixtureRequest()
			app := req.Application
			appReads, projectReads := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/api/v1/applications/"):
					appReads++
					spec := domain.CloneSpec(app.Spec)
					if stage == "application" && appReads > 1 {
						spec["project"] = "changed"
					}
					json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": app.ID.Name, "namespace": app.ID.Namespace}, "spec": spec})
				case r.URL.Path == "/api/v1/projects/demo":
					projectReads++
					spec := map[string]any{"sourceRepos": []any{"*"}, "destinations": []any{map[string]any{"name": "*", "namespace": "*"}}}
					if stage == "project" && projectReads > 1 {
						spec["description"] = "changed"
					}
					json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "demo"}, "spec": spec})
				case r.URL.Path == "/api/v1/settings":
					json.NewEncoder(w).Encode(req.Settings)
				case strings.HasPrefix(r.URL.Path, "/api/v1/clusters/"):
					json.NewEncoder(w).Encode(map[string]any{"server": "https://cluster.example.invalid", "name": "service-east", "info": map[string]any{"serverVersion": "1.31.0", "apiVersions": []string{"v1", "apps/v1/Deployment"}, "connectionState": map[string]any{"status": "Successful"}}})
				default:
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			files := req.Repositories[testRepo+"@"+testSHA]
			files["app/templates/broken.yaml"] = []byte(`{{ mustExampleIPs "demo" }}`)
			client := &Client{API: &argocd.Client{Instance: app.ID.Instance, HTTP: httpx.New(server.URL, "fictional-token", time.Second, 1<<20)}, Git: fixtureGit{files: map[string]map[string][]byte{"example/charts@" + testSHA: files}}, RepositoryURL: testRepo, Limit: 1 << 20, Execute: func(_ context.Context, r Request) ([]string, error) { return Render(r) }}
			out, err := client.Render(context.Background(), app, testSHA)
			if len(out) != 0 || err == nil || domain.IsRenderPartial(err) || !strings.Contains(err.Error(), "render_context_changed") {
				t.Fatalf("partial targets bypassed drift check: %v %v", out, err)
			}
		})
	}
}

func TestSourceSnapshotLimitIsConfigurable(t *testing.T) {
	base, head := testSHA, strings.Repeat("b", 40)
	payload := func() map[string][]byte {
		return map[string][]byte{"app/Chart.yaml": bytes.Repeat([]byte("x"), 40<<10)}
	}
	git := fixtureGit{files: map[string]map[string][]byte{"example/charts@" + base: payload(), "example/charts@" + head: payload()}}

	// Both revisions fit only when the configured limit covers their sum.
	c := &Client{Git: git, SnapshotLimit: 64 << 10}
	if _, _, err := c.archive(context.Background(), "example/charts", base); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.archive(context.Background(), "example/charts", head)
	var failure *domain.Failure
	if !errors.As(err, &failure) || failure.Issue.Code != "limit_exceeded" || !strings.Contains(failure.Issue.Action, "render.maxSourceSnapshotBytes") {
		t.Fatal("snapshot limit was not enforced with an actionable setting", err)
	}

	c = &Client{Git: git, SnapshotLimit: 96 << 10}
	for _, sha := range []string{base, head} {
		if _, _, err := c.archive(context.Background(), "example/charts", sha); err != nil {
			t.Fatal("configured snapshot limit rejected sources within it", err)
		}
	}
	if (&Client{}).snapshotLimit() != 64<<20 {
		t.Fatal("unset snapshot limit changed the 64 MiB default")
	}
}

func TestRepositoryAliasReadsSelectedRepositoryThroughConfiguredAPI(t *testing.T) {
	c := &Client{RepositoryURL: "https://github.example.com/example/charts", RepositoryAliases: []string{"https://mirror.example.com/example/charts"}}
	for _, source := range []string{"https://github.example.com/example/charts", "https://mirror.example.com/example/charts.git", "git@mirror.example.com:example/charts.git"} {
		if repo, err := c.repository(source); err != nil || repo != "example/charts" {
			t.Fatalf("%s mapped to %q: %v", source, repo, err)
		}
	}
	// Other repositories on the alias host are not implied by the alias.
	var skipped *domain.RenderSkipped
	for _, source := range []string{"https://mirror.example.com/example/other", "https://attacker.example/example/charts"} {
		if _, err := c.repository(source); !errors.As(err, &skipped) {
			t.Fatalf("%s was accepted: %v", source, err)
		}
	}
}

func TestMirrorHostReadsOtherRepositoriesThroughConfiguredAPI(t *testing.T) {
	c := &Client{RepositoryURL: "https://github.example.com/example/charts", MirrorHosts: []string{"https://mirror.example.com"}}
	if repo, err := c.repository("https://mirror.example.com/example/templates.git"); err != nil || repo != "example/templates" {
		t.Fatalf("mirrored source mapped to %q: %v", repo, err)
	}
	var skipped *domain.RenderSkipped
	for _, source := range []string{"https://other.example.com/example/templates", "http://mirror.example.com/example/templates"} {
		if _, err := c.repository(source); !errors.As(err, &skipped) {
			t.Fatalf("%s was accepted: %v", source, err)
		}
	}
}

// A panic in the worker's render reaches the parent as a panic failure with
// its stack, not as a generic render failure, and withholds the panic value.
func TestWorkerReportsRenderPanicAsStructuredIssue(t *testing.T) {
	saved := workerRender
	defer func() { workerRender = saved }()
	workerRender = func(Request) ([]string, error) { panic("fictional-secret-value") }
	req := fixtureRequest()
	wire := workerRequest{Request: req, Sources: []map[string]any{req.Plan[0].Source}}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if code := Worker(bytes.NewReader(data), &output); code != 0 {
		t.Fatalf("worker exit %d", code)
	}
	if strings.Contains(output.String(), "fictional-secret-value") {
		t.Fatal("panic value reached the worker response")
	}
	var out response
	if err := json.Unmarshal(output.Bytes(), &out); err != nil || out.Issue == nil {
		t.Fatalf("response %s: %v", output.String(), err)
	}
	if out.Issue.ReasonCode != domain.ReasonPanic || out.Issue.Diagnostic == nil || !strings.Contains(out.Issue.Diagnostic.Stack[0], "TestWorkerReportsRenderPanicAsStructuredIssue") || out.Reason == "" {
		t.Fatalf("issue = %+v reason=%q", out.Issue, out.Reason)
	}
}
