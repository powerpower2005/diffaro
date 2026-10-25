// Package demo runs trusted embedded fixtures through the production preview
// and Helm engines. It never constructs network clients or reads credentials.
package demo

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
	"diffaro/internal/localhelm"
	"diffaro/internal/preview"
	"diffaro/internal/profile"
)

//go:embed fixtures/*/* fixtures/*/templates/*
var fixtures embed.FS

const repository = "https://github.com/example/diffaro-demo.git"
const baseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const headSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// Scenarios lists the built-in demo scenarios in display order.
var Scenarios = []string{"new-app", "partial-failure", "skipped-context"}

// CheckScenario rejects a scenario that is not built in.
func CheckScenario(scenario string) error {
	if slices.Contains(Scenarios, scenario) {
		return nil
	}
	last := len(Scenarios) - 1
	return fmt.Errorf("scenario must be %s or %s", strings.Join(Scenarios[:last], ", "), Scenarios[last])
}

// Run compares synthetic immutable inputs. Its markers must stay explicit in
// every output; the report does not establish a user's Argo CD compatibility.
func Run(ctx context.Context, scenario string) (domain.Report, error) {
	if err := CheckScenario(scenario); err != nil {
		return domain.Report{}, err
	}
	files := map[string][]byte{}
	if err := fs.WalkDir(fixtures, "fixtures", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fixtures.ReadFile(path)
		if err == nil {
			files[path[len("fixtures/"):]] = data
		}
		return err
	}); err != nil {
		return domain.Report{}, fmt.Errorf("embedded demo fixtures unavailable")
	}
	b := &backend{scenario: scenario, files: files, roots: map[domain.ApplicationID]domain.Application{}}
	root := application("platform", "parent", "argocd")
	b.roots[root.ID] = root
	c := profile.Config{Git: profile.Git{RepositoryURL: repository},
		Instances:    []profile.Instance{{ID: "demo", LocalHelm: true}},
		Applications: []domain.ApplicationID{root.ID}, Routing: profile.Routing{DefaultInstance: "demo"},
		Render: profile.Render{Timeout: "5s", MaxConcurrency: 1, MaxDepth: 8, MaxApplications: 10, MaxResponseBytes: 1 << 20, MaxDocuments: 50}}
	if scenario == "partial-failure" {
		broken := application("broken", "broken", "demo")
		b.roots[broken.ID] = broken
		c.Applications = append(c.Applications, broken.ID)
	}
	runner := preview.Runner{Config: c, Renderers: map[string]domain.Renderer{"demo": b}, BackendLimitations: preview.BackendLimitations{
		BasicAnalysis: "Demo renders supported desired child specs through bundled Helm; skipped-context deliberately withholds a child target and its descendant discovery.",
		SecretPayload: "Demo uses fixed synthetic API capabilities; it does not verify an organization's Argo CD context. Production redaction and target coverage rules still apply.",
	}}
	report := runner.RunRecursive(ctx, domain.RevisionPair{Repository: "example/diffaro-demo", Base: baseSHA, Head: headSHA, Mode: "demo-fixture-base-head"})
	// The demo has no pull request or backend notes; the production steps
	// after every comparison still apply.
	runner.CompleteComparison(ctx, &report, preview.ComparisonEvidence{WithoutInputs: true})
	report.Limitations = append([]string{"DEMO: synthetic Git revisions, Application lookup and API context; actual bundled Helm and production graph/diff/report engines. No credentials, GitHub, Argo CD or Kubernetes connection is used."}, report.Limitations...)
	report.RenderBackends = []domain.BackendInfo{{Instance: "demo", Kind: domain.BackendLocalHelm, ContextID: "embedded-demo-context/helm-" + localhelm.HelmVersion, Verification: "demo-fixture", Compatibility: "demo-only"}}
	return report, nil
}

func application(name, path, namespace string) domain.Application {
	return domain.Application{ID: domain.ApplicationID{Instance: "demo", Namespace: "argocd", Name: name}, Spec: map[string]any{
		"project": "demo", "source": map[string]any{"repoURL": repository, "path": path, "targetRevision": "main", "helm": map[string]any{"releaseName": name}},
		"destination": map[string]any{"name": "demo-cluster", "namespace": namespace}}}
}

type backend struct {
	scenario string
	files    map[string][]byte
	roots    map[domain.ApplicationID]domain.Application
}

func (b *backend) Application(ctx context.Context, id domain.ApplicationID) (domain.Application, error) {
	if err := ctx.Err(); err != nil {
		return domain.Application{}, err
	}
	a, ok := b.roots[id]
	if !ok {
		return domain.Application{}, domain.Fail(domain.CodeNotFound, "lookup", "demo root is unavailable", "use a built-in demo scenario", nil)
	}
	return domain.Application{ID: a.ID, Spec: domain.CloneSpec(a.Spec)}, nil
}

func (b *backend) RenderDesired(ctx context.Context, a domain.Application, sha string) ([]string, error) {
	if b.scenario == "skipped-context" && a.ID.Name == "worker" {
		return nil, &domain.RenderSkipped{Reason: "Demo: cached API capabilities for the worker destination are unavailable. Provide cluster read access and refresh this Argo CD instance's destination discovery."}
	}
	return b.Render(ctx, a, sha)
}

func (b *backend) Render(ctx context.Context, a domain.Application, sha string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sha != baseSHA && sha != headSHA {
		return nil, domain.Fail(domain.CodeRevisionInvalid, "revision", "unknown demo revision", "use a built-in demo scenario", nil)
	}
	files := make(map[string][]byte, len(b.files))
	for path, data := range b.files {
		files[path] = data
	}
	if sha == headSHA {
		files["parent/values.yaml"] = []byte("workerEnabled: true\nreplicas: 1\nselector: missing\nconfig: after\nmaxUnavailable: 1\n")
		if b.scenario == "partial-failure" {
			files["broken/templates/config.yaml"] = []byte(`{{ fail "demo invalid chart" }}`)
		}
	}
	source := domain.CloneSpec(a.Source())
	source["targetRevision"] = sha
	return localhelm.Render(localhelm.Request{Application: a, Plan: []domain.SourceRevision{{Position: 1, Repository: repository, Revision: sha, Source: source}},
		Settings:     argocd.RenderSettings{AppLabelKey: "app.kubernetes.io/instance", TrackingMethod: "annotation", ControllerNamespace: "argocd"},
		Capabilities: argocd.ClusterCapabilities{Version: "1.31.0", APIVersions: []string{"v1", "apps/v1/Deployment"}},
		Repositories: map[string]map[string][]byte{repository + "@" + sha: files}, Limit: 1 << 20})
}
