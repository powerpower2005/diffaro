package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diffaro/internal/profile"
)

func TestCLIRepositoryDiscoveryWithoutRoot(t *testing.T) {
	t.Run("https", func(t *testing.T) { testCLIRepositoryDiscovery(t, false) })
	t.Run("ssh", func(t *testing.T) { testCLIRepositoryDiscovery(t, true) })
}

func testCLIRepositoryDiscovery(t *testing.T, ssh bool) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	application := testApp()
	if ssh {
		application["spec"].(map[string]any)["source"].(map[string]any)["repoURL"] = "git@github.com:example/charts.git"
	}
	reads := 0
	routes := map[string]http.HandlerFunc{"/api/v1/applications": func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fictional-argo-0" {
			t.Error("inventory must use instance credentials")
		}
		reads++
		writeInventory(w, application)
	}}
	if ssh {
		routes["/api/v1/applications/api"] = func(w http.ResponseWriter, r *http.Request) { writeJSON(w, application) }
		routes["/api/v1/projects/demo"] = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"metadata": map[string]any{"name": "demo"}, "spec": map[string]any{"sourceRepos": []any{"git@github.com:example/charts.git"}}})
		}
	}
	server := overlayArgoCD(t, a, routes)
	cfg := configFile(t, g.URL, server.URL)
	editProfile(t, cfg, "applications:\n  - instance: instance-0\n    namespace: argocd\n    name: api\n", "discovery:\n  mode: repository\nrouting:\n  defaultInstance: instance-0\n")
	report, code := runJSON(t, cfg, "--pr", "https://github.com/example/charts/pull/12")
	if code != 1 || len(report.Applications) != 1 || len(report.Discovery) != 1 || report.Discovery[0].Status != "complete" || reads != 2 {
		t.Fatalf("automatic PR discovery failed: code=%d reads=%d report=%+v", code, reads, report)
	}
	if report.Revisions.Base != ancestorSHA || report.Coverage != "repository-discovered-target-graph" {
		t.Fatal("PR semantics or discovery coverage lost")
	}
	// The same result must be reachable without hand-authoring a profile or
	// choosing an Application name.
	initialized := filepath.Join(t.TempDir(), "discovered.yaml")
	var out, diagnostic bytes.Buffer
	args := []string{"init", "--endpoint", server.URL, "--instance", "instance-0", "--pr", "https://github.com/example/charts/pull/12", "--config", initialized, "--single-instance", "--git-api", g.URL, "--token-env", "DIFFARO_TEST_ARGO_0", "--git-token-env", "DIFFARO_TEST_GITHUB"}
	if code := Run(context.Background(), args, &out, &diagnostic); code != 0 {
		t.Fatalf("init from PR failed: %d %s", code, diagnostic.String())
	}
	report, code = runJSON(t, initialized, "--pr", "https://github.com/example/charts/pull/12")
	if code != 1 || len(report.Applications) != 1 || report.Discovery[0].Status != "complete" {
		t.Fatal("initialized discovery profile did not compare targets")
	}
	before, _ := os.ReadFile(initialized)
	assertNoSecrets(t, "init profile with Application render inputs", string(before), "fictional-inline-value", "fictional-object-value")
	if code := Run(context.Background(), args, &out, &diagnostic); code != 4 {
		t.Fatal("init overwrote existing profile")
	}
	autoPath := filepath.Join(t.TempDir(), "automatic.yaml")
	autoArgs := append([]string{}, args...)
	for i := range autoArgs {
		if autoArgs[i] == initialized {
			autoArgs[i] = autoPath
		}
	}
	autoArgs = append(autoArgs, "--repo-server", "127.0.0.1:8081", "--repo-server-plaintext", "--grpcurl", "tools/grpcurl", "--kubectl", "tools/kubectl", "--kubeconfig", "local-cluster.yaml", "--kube-context", "verified-local")
	if code := Run(context.Background(), autoArgs, &out, &diagnostic); code != 0 {
		t.Fatalf("automatic context init: %d %s", code, diagnostic.String())
	}
	config, err := profile.Load(autoPath)
	if err != nil {
		t.Fatal(err)
	}
	rpc := config.Instances[0].RepoServer
	if rpc == nil || rpc.ContextFile != "" || rpc.Kubernetes == nil || rpc.Kubernetes.Context != "verified-local" || !filepath.IsAbs(rpc.Kubernetes.Kubeconfig) {
		t.Fatal("automatic connection settings lost")
	}
	forwardPath := filepath.Join(t.TempDir(), "forward.yaml")
	forwardArgs := []string{}
	for i := 0; i < len(autoArgs); i++ {
		if autoArgs[i] == "--repo-server" {
			forwardArgs = append(forwardArgs, "--repo-server-forward")
			i++
			continue
		}
		if autoArgs[i] == autoPath {
			forwardArgs = append(forwardArgs, forwardPath)
			continue
		}
		forwardArgs = append(forwardArgs, autoArgs[i])
	}
	if code := Run(context.Background(), forwardArgs, &out, &diagnostic); code != 0 {
		t.Fatalf("forward init: %d %s", code, diagnostic.String())
	}
	forwardConfig, err := profile.Load(forwardPath)
	if err != nil {
		t.Fatal(err)
	}
	forward := forwardConfig.Instances[0].RepoServer
	if forward.Address != "" || forward.PortForward == nil || forward.PortForward.Resource != "service/argocd-repo-server" || forward.PortForward.Port != 8081 {
		t.Fatal("managed forward configuration lost")
	}
}

// Argo CD may read the PR repository through a mirror URL. Only an explicit
// alias selects it; the PR URL, API and credentials stay on the origin.
func TestCLIRepositoryDiscoveryThroughExplicitMirrorAlias(t *testing.T) {
	const mirror = "https://mirror.example.com/example/charts"
	t.Setenv("DIFFARO_TEST_GITHUB", "fictional-github-token")
	t.Setenv("DIFFARO_TEST_ARGO_0", "fictional-argo-0")
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	application := testApp()
	application["spec"].(map[string]any)["source"].(map[string]any)["repoURL"] = mirror
	server := overlayArgoCD(t, a, map[string]http.HandlerFunc{
		"/api/v1/applications":     func(w http.ResponseWriter, r *http.Request) { writeInventory(w, application) },
		"/api/v1/applications/api": func(w http.ResponseWriter, r *http.Request) { writeJSON(w, application) },
		"/api/v1/projects/demo": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"metadata": map[string]any{"name": "demo"}, "spec": map[string]any{"sourceRepos": []any{mirror}}})
		},
	})
	run := func(extra ...string) (string, int, string) {
		path := filepath.Join(t.TempDir(), "profile.yaml")
		args := append([]string{"init", "--endpoint", server.URL, "--instance", "instance-0", "--pr", "https://github.com/example/charts/pull/12", "--config", path, "--single-instance", "--git-api", g.URL, "--token-env", "DIFFARO_TEST_ARGO_0", "--git-token-env", "DIFFARO_TEST_GITHUB"}, extra...)
		var out, diagnostic bytes.Buffer
		code := Run(context.Background(), args, &out, &diagnostic)
		return path, code, diagnostic.String()
	}
	initialize := func(extra ...string) string {
		path, code, diagnostic := run(extra...)
		if code != 0 {
			t.Fatalf("init failed: %d %s", code, diagnostic)
		}
		return path
	}

	// Without the alias or mirror host the profile would select nothing, so
	// init writes none and names the setting.
	path, exit, refusal := run()
	if _, err := os.Stat(path); exit != 4 || err == nil || !strings.Contains(refusal, "No profile written") || !strings.Contains(refusal, mirror) || !strings.Contains(refusal, "--mirror-host") {
		t.Fatalf("init wrote a profile that selects no Application: code=%d stat=%v\n%s", exit, err, refusal)
	}

	aliased := initialize("--repository-alias", mirror)
	config, err := profile.Load(aliased)
	if err != nil || len(config.Git.RepositoryAliases) != 1 || config.Git.RepositoryAliases[0] != mirror || config.Git.RepositoryURL != "https://github.com/example/charts" {
		t.Fatal("init did not save the alias next to the origin repository", err)
	}
	// Discovery itself never selects an undeclared mirror source.
	undeclared := filepath.Join(t.TempDir(), "undeclared.yaml")
	data, err := os.ReadFile(aliased)
	if err != nil || os.WriteFile(undeclared, data, 0o600) != nil {
		t.Fatal("cannot copy the profile", err)
	}
	editProfile(t, undeclared, "    repositoryAliases:\n        - "+mirror+"\n", "")
	report, code := runJSON(t, undeclared, "--pr", "https://github.com/example/charts/pull/12")
	if code != 0 || len(report.Applications) != 0 || report.Discovery[0].Status != "no_matches" {
		t.Fatalf("mirror source was selected without an alias: code=%d %+v", code, report.Discovery)
	}
	report, code = runJSON(t, aliased, "--pr", "https://github.com/example/charts/pull/12")
	if code != 1 || len(report.Applications) != 1 || report.Discovery[0].Status != "complete" || report.Revisions.Base != ancestorSHA {
		t.Fatalf("aliased mirror source was not compared at the PR revisions: code=%d %+v", code, report.Discovery)
	}

	mirrorHost := strings.TrimSuffix(mirror, "/example/charts.git")
	mirrorHost = strings.TrimSuffix(mirrorHost, "/example/charts")
	mirrored := initialize("--mirror-host", mirrorHost)
	config, err = profile.Load(mirrored)
	if err != nil || len(config.Git.MirrorHosts) != 1 || config.Git.MirrorHosts[0] != mirrorHost {
		t.Fatal("init did not save the mirror host", err)
	}
	report, code = runJSON(t, mirrored, "--pr", "https://github.com/example/charts/pull/12")
	if code != 1 || len(report.Applications) != 1 || report.Discovery[0].Status != "complete" {
		t.Fatalf("mirror-host source was not compared at the PR revisions: code=%d %+v", code, report.Discovery)
	}

	var out, diagnostic bytes.Buffer
	if code := Run(context.Background(), []string{"init", "--endpoint", server.URL, "--application", "argocd/api", "--repository-alias", mirror, "--config", filepath.Join(t.TempDir(), "x.yaml")}, &out, &diagnostic); code != 4 {
		t.Fatal("--repository-alias without --pr was accepted", code)
	}
	if code := Run(context.Background(), []string{"init", "--endpoint", server.URL, "--pr", "https://github.com/example/charts/pull/12", "--git-api", g.URL, "--repository-alias", "https://mirror.example.com/example/other", "--config", filepath.Join(t.TempDir(), "y.yaml")}, &out, &diagnostic); code == 0 {
		t.Fatal("alias for another repository was accepted")
	}
}

// Opt-in changed-path selection renders a candidate only when the PR touches
// its source path; otherwise the candidate is recorded as unaffected.
func TestCLIChangedPathSelection(t *testing.T) {
	t.Setenv("DIFFARO_TEST_GITHUB", "fictional-github-token")
	t.Setenv("DIFFARO_TEST_ARGO_0", "fictional-argo-0")
	for _, tc := range []struct {
		name       string
		changed    string
		status     string
		code       int
		apps       int
		unaffected int
		link       bool
		archive    bool
		// selection is the init --selection value; empty takes the init
		// default.
		selection string
	}{
		{"outside source path", "charts/other/values.yaml", "modified", 0, 0, 1, false, false, "changedPaths"},
		{"outside source path, metadata from the archive", "charts/other/values.yaml", "modified", 0, 0, 1, false, true, "changedPaths"},
		{"inside source path", "charts/api/values.yaml", "modified", 1, 1, 0, false, false, "changedPaths"},
		{"chart file added", "charts/api/templates/job.yaml", "added", -1, 1, 0, false, false, "changedPaths"},
		{"symbolic link in chart", "charts/other/values.yaml", "modified", -1, 1, 0, true, false, "changedPaths"},
		{"init default selection", "charts/other/values.yaml", "modified", 0, 0, 1, false, true, ""},
		{"init selection all", "charts/other/values.yaml", "modified", 1, 1, 0, false, true, "all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contents, archives := 0, 0
			g := newGitHubFake(t, githubFake{token: "fictional-github-token", routes: func(w http.ResponseWriter, r *http.Request) bool {
				switch {
				case r.URL.Path == "/repos/example/charts/pulls/12" && r.URL.Query().Get("page") == "":
					// The PR lookup for revisions ignores changed_files.
					fmt.Fprintf(w, `{"changed_files":1,"base":{"sha":%q,"repo":{"full_name":"example/charts"}},"head":{"sha":%q,"repo":{"full_name":"example/charts"}}}`, baseSHA, headSHA)
				case r.URL.Path == "/repos/example/charts/pulls/12/files":
					fmt.Fprintf(w, `[{"filename":%q,"status":%q}]`, tc.changed, tc.status)
				case r.URL.Path == "/repos/example/charts/tarball/"+headSHA:
					// One archive read serves the selection's chart metadata.
					archives++
					if !tc.archive {
						http.NotFound(w, r)
						return true
					}
					gz := gzip.NewWriter(w)
					tw := tar.NewWriter(gz)
					chart := "apiVersion: v2\nname: api\nversion: 0.1.0\n"
					_ = tw.WriteHeader(&tar.Header{Name: "archive/charts/api/Chart.yaml", Mode: 0600, Size: int64(len(chart))})
					_, _ = tw.Write([]byte(chart))
					_ = tw.Close()
					_ = gz.Close()
				case r.URL.Path == "/repos/example/charts/git/trees/"+headSHA:
					link := ""
					if tc.link {
						link = `,{"path":"charts/api/templates/shared.yaml","mode":"120000","type":"blob","sha":"` + strings.Repeat("c", 40) + `"}`
					}
					fmt.Fprintf(w, `{"truncated":false,"tree":[{"path":"charts/api/Chart.yaml","mode":"100644","type":"blob","sha":%q}%s]}`, strings.Repeat("b", 40), link)
				case strings.HasPrefix(r.URL.Path, "/repos/example/charts/contents/"):
					// Input-change evidence also reads rendered candidates' files at the merge base.
					ref := r.URL.Query().Get("ref")
					switch ref {
					case headSHA:
						contents++
					case ancestorSHA:
					default:
						t.Error("chart metadata read from a mutable or wrong revision")
					}
					if r.URL.Path == "/repos/example/charts/contents/charts/api/values.yaml" {
						values := "replicaCount: 1\n"
						if ref == headSHA {
							values = "replicaCount: 2\n"
						}
						fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte(values)))
						return true
					}
					if r.URL.Path != "/repos/example/charts/contents/charts/api/Chart.yaml" {
						http.NotFound(w, r)
						return true
					}
					fmt.Fprint(w, `{"type":"file","encoding":"base64","content":"YXBpVmVyc2lvbjogdjIKbmFtZTogYXBpCnZlcnNpb246IDAuMS4wCg=="}`)
				default:
					return false
				}
				return true
			}})
			a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
			application := testApp()
			application["status"] = map[string]any{"resources": []any{map[string]any{"group": "apps", "kind": "Deployment"}}}
			server := overlayArgoCD(t, a, map[string]http.HandlerFunc{"/api/v1/applications": func(w http.ResponseWriter, r *http.Request) { writeInventory(w, application) }})
			path := filepath.Join(t.TempDir(), "profile.yaml")
			var out, diagnostic bytes.Buffer
			args := []string{"init", "--endpoint", server.URL, "--instance", "instance-0", "--pr", "https://github.com/example/charts/pull/12", "--config", path, "--single-instance", "--git-api", g.URL, "--token-env", "DIFFARO_TEST_ARGO_0", "--git-token-env", "DIFFARO_TEST_GITHUB"}
			if tc.selection != "" {
				args = append(args, "--selection", tc.selection)
			}
			if code := Run(context.Background(), args, &out, &diagnostic); code != 0 {
				t.Fatalf("init failed: %d %s", code, diagnostic.String())
			}
			report, code := runJSON(t, path, "--pr", "12")
			if (tc.code >= 0 && code != tc.code) || len(report.Applications) != tc.apps || report.Discovery[0].Unaffected != tc.unaffected || report.Discovery[0].Status != "complete" {
				t.Fatalf("code=%d apps=%d discovery=%+v", code, len(report.Applications), report.Discovery)
			}
			decision := report.Discovery[0].Decisions[0]
			if tc.selection == "all" {
				// Every candidate renders, and no changed-path decision exists.
				if decision.Selection != "" {
					t.Fatalf("selection all made a changed-path decision: %+v", decision)
				}
				return
			}
			if decision.Selection == "" || decision.SelectionReason == "" {
				t.Fatalf("selection reason missing: %+v", decision)
			}
			if tc.changed == "charts/api/values.yaml" {
				inputs := report.Applications[0].InputChanges
				if len(inputs) != 1 || inputs[0].Path != "charts/api/values.yaml" || len(inputs[0].Changes) != 1 || inputs[0].Changes[0].Path != "/replicaCount" {
					t.Fatalf("PR value file change must be listed with its key: %+v", inputs)
				}
			}
			if tc.status == "added" {
				// The PR file status reaches the path-only chart file entry.
				if inputs := report.Applications[0].InputChanges; len(inputs) != 1 || inputs[0].Path != tc.changed || inputs[0].Kind != "chartFile" || inputs[0].State != "added" {
					t.Fatalf("added chart file must be listed as added: %+v", inputs)
				}
			}
			if tc.unaffected == 1 && tc.archive && (contents != 0 || archives != 1) {
				t.Fatalf("chart metadata: %d file reads and %d archive reads; want one archive read", contents, archives)
			}
			if tc.unaffected == 1 && !tc.archive && contents != 1 {
				t.Fatalf("chart metadata reads: %d; absent requirements.yaml must not be read, and an unavailable archive falls back to file reads", contents)
			}
			// Explicit base/head has no PR file list; every candidate renders.
			report, code = runJSON(t, path, "--base", "main", "--head", "feature")
			if len(report.Applications) != 1 || report.Discovery[0].Unaffected != 0 {
				t.Fatalf("explicit revisions skipped a candidate: code=%d %+v", code, report.Discovery)
			}
		})
	}
	var out, diagnostic bytes.Buffer
	if code := Run(context.Background(), []string{"init", "--endpoint", "https://argo.example.invalid", "--application", "argocd/api", "--selection", "changedPaths", "--config", filepath.Join(t.TempDir(), "x.yaml")}, &out, &diagnostic); code != 4 {
		t.Fatal("--selection without --pr was accepted", code)
	}
}

// A run whose PR lookup fails renders nothing, but its report must still name
// the coverage of the requested mode rather than the explicit-root default.
func TestRevisionFailureKeepsRequestedCoverage(t *testing.T) {
	g := gitServer(t)
	a, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	explicit := configFile(t, g.URL, a.URL)
	discovered := configFile(t, g.URL, a.URL)
	editProfile(t, discovered, "applications:\n  - instance: instance-0\n    namespace: argocd\n    name: api\n", "discovery:\n  mode: repository\nrouting:\n  defaultInstance: instance-0\n")
	for _, tc := range []struct {
		name, config, coverage string
		args                   []string
	}{
		{"explicit", explicit, "explicit-applications", nil},
		{"recursive", explicit, "recursive-target-graph", []string{"--recursive"}},
		{"repository discovery", discovered, "repository-discovered-target-graph", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, code := runJSON(t, tc.config, append([]string{"--pr", "https://github.com/example/charts/pull/99"}, tc.args...)...)
			if code != 6 || report.Status != "failed" || len(report.Applications) != 0 || len(report.Issues) != 1 || report.Issues[0].Stage == "" {
				t.Fatalf("missing PR was not an explicit failure: code=%d report=%+v", code, report)
			}
			if report.Coverage != tc.coverage {
				t.Fatalf("coverage = %q, want the requested mode %q", report.Coverage, tc.coverage)
			}
		})
	}
}
