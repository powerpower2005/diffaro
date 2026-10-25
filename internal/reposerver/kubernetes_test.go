package reposerver

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

func TestAutomaticContextPreservesSettingsAndCapabilities(t *testing.T) {
	k := newKubernetesContext(profile.KubernetesContext{Namespace: "argocd"})
	calls := 0
	k.read = func(_ context.Context, args ...string) (map[string]any, error) {
		calls++
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "configmap argocd-cm"):
			return map[string]any{"data": map[string]any{"application.resourceTrackingMethod": "annotation", "application.instanceLabelKey": "example.com/instance", "installationID": "installation", "helm.valuesFileSchemes": "https, custom"}}, nil
		case strings.Contains(joined, "secret-type=repository"):
			return map[string]any{"items": []any{map[string]any{"data": map[string]any{"url": base64.StdEncoding.EncodeToString([]byte(repository)), "password": base64.StdEncoding.EncodeToString([]byte("fictional-private-password"))}}}}, nil
		case strings.Contains(joined, "secret-type=repo-creds"):
			return map[string]any{"items": []any{}}, nil
		case joined == "get --raw /version":
			return map[string]any{"gitVersion": "v1.31.0"}, nil
		case joined == "get --raw /api":
			return map[string]any{"versions": []any{"v1"}}, nil
		case joined == "get --raw /apis":
			return map[string]any{"groups": []any{}}, nil
		case joined == "get --raw /api/v1":
			return map[string]any{"resources": []any{map[string]any{"kind": "ConfigMap"}}}, nil
		default:
			t.Fatalf("unexpected context command %q", joined)
			return nil, nil
		}
	}
	snapshot, app, project := fixture()
	app.Spec["destination"] = map[string]any{"server": "https://kubernetes.default.svc", "namespace": "new-namespace"}
	project["destinations"] = []any{app.Spec["destination"]}
	binding, err := k.binding(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Bindings = []Binding{binding}
	if err := snapshot.Validate("test"); err != nil {
		t.Fatal(err)
	}
	if binding.Applications != nil || domain.String(binding.Request, "appLabelKey") != "example.com/instance" || domain.String(binding.Request, "installationID") != "installation" || !reflect.DeepEqual(binding.Request["apiVersions"], []any{"v1", "v1/ConfigMap"}) {
		t.Fatal("trusted settings or scope lost")
	}
	client := &Client{API: fakeAPI(t, func(int) map[string]any { return project }), snapshot: snapshot, kubernetes: k}
	client.snapshot.Bindings = nil
	k.secrets = append(k.secrets, map[string]string{"_kind": "repository", "type": "helm", "url": "https://charts.example.com", "username": "fixture", "password": "fictional-dependency-password"})
	project["sourceRepos"] = append(project["sourceRepos"].([]any), "https://charts.example.com")
	client.invoke = func(_ context.Context, req map[string]any) (Response, error) {
		if req["appName"] != app.ID.Name || req["namespace"] != "new-namespace" || req["repo"].(map[string]any)["password"] != "fictional-private-password" {
			t.Fatal("desired request or credentials lost")
		}
		dependencies := req["repos"].([]any)
		if len(dependencies) != 1 || dependencies[0].(map[string]any)["password"] != "fictional-dependency-password" {
			t.Fatal("authorized dependency credentials did not reach the render backend")
		}
		return Response{Revision: commit, SourceType: "Helm", Manifests: []string{}}, nil
	}
	if _, err := client.RenderDesired(context.Background(), app, commit); err != nil {
		t.Fatal(err)
	}
	if calls != 7 {
		t.Fatalf("expected one complete context collection, got %d reads", calls)
	}
	app.Spec["destination"] = map[string]any{"server": "https://external.example.com", "namespace": "demo"}
	if _, err := k.binding(context.Background(), app); err == nil {
		t.Fatal("external cluster reused in-cluster capabilities")
	}
}

func TestAutomaticContextFailureDoesNotCachePartialInputs(t *testing.T) {
	k := newKubernetesContext(profile.KubernetesContext{})
	k.read = func(context.Context, ...string) (map[string]any, error) { return nil, errors.New("read failed") }
	if k.load(context.Background()) == nil || k.settings != nil || k.secrets != nil {
		t.Fatal("failed capture retained partial state")
	}
}

func TestRepositoryCredentialSelection(t *testing.T) {
	secrets := []map[string]string{
		{"_kind": "repo-creds", "url": "https://github.com/", "password": "broad"},
		{"_kind": "repo-creds", "url": "https://github.com/example/", "password": "narrow"},
		{"_kind": "repository", "url": repository, "project": "another", "password": "other-project"},
	}
	got, err := selectRepository(secrets, repository, "demo")
	if err != nil || got["password"] != "narrow" || got["repo"] != repository {
		t.Fatal("credential scope selection failed")
	}
	secrets = append(secrets, map[string]string{"_kind": "repository", "url": repository, "project": "demo", "password": "own"})
	got, err = selectRepository(secrets, repository, "demo")
	if err != nil || got["password"] != "own" {
		t.Fatal("explicit credentials must win")
	}
	secrets = append(secrets, map[string]string{"_kind": "repository", "url": repository, "password": "ambiguous"})
	if _, err := selectRepository(secrets, repository, "demo"); err == nil {
		t.Fatal("ambiguous repository credentials accepted")
	}
}

func TestAutomaticContextUsesEffectiveTrackingDefault(t *testing.T) {
	_, app, _ := fixture()
	app.Spec["destination"] = map[string]any{"server": "https://kubernetes.default.svc", "namespace": "demo"}
	for _, method := range []string{"label", "annotation", "annotation+label", "", "unknown"} {
		t.Run(method, func(t *testing.T) {
			k := &kubernetesContext{settings: map[string]any{}, version: "v1.31.0", apis: []any{"v1"}, tracking: func(context.Context) (string, error) { return method, nil }}
			binding, err := k.binding(context.Background(), app)
			if method == "" || method == "unknown" {
				if err == nil {
					t.Fatal("missing or unknown default accepted")
				}
				return
			}
			if err != nil || binding.Request["trackingMethod"] != method {
				t.Fatalf("effective tracking lost: %v", err)
			}
			k.tracking = func(context.Context) (string, error) { return "", errors.New("must reuse run snapshot") }
			binding, err = k.binding(context.Background(), app)
			if err != nil || binding.Request["trackingMethod"] != method {
				t.Fatal("tracking context changed between target renders")
			}
		})
	}
	k := &kubernetesContext{settings: map[string]any{}, tracking: func(context.Context) (string, error) { return "", errors.New("denied") }}
	if _, err := k.binding(context.Background(), app); err == nil {
		t.Fatal("tracking lookup failure hidden")
	}
}

func TestInheritedCredentialsPreserveRepositoryConfiguration(t *testing.T) {
	secrets := []map[string]string{
		{"_kind": "repository", "url": repository, "proxy": "https://repository-proxy.example", "gcpServiceAccountKey": "existing-gcp", "githubAppID": "42", "forceHttpBasicAuth": "true"},
		{"_kind": "repo-creds", "url": "https://github.com/example/", "password": "inherited", "proxy": "https://template-proxy.example", "gcpServiceAccountKey": "template-gcp", "githubAppID": "99"},
	}
	got, err := selectRepository(secrets, repository, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if got["proxy"] != "https://repository-proxy.example" || got["gcpServiceAccountKey"] != "existing-gcp" || got["githubAppID"] != int64(42) || got["password"] != "inherited" || got["forceHttpBasicAuth"] != false {
		t.Fatalf("credential inheritance differs from upstream: keys=%d", len(got))
	}
	secrets[1]["useAzureWorkloadIdentity"] = "true"
	if _, err := selectRepository(secrets, repository, "demo"); err == nil {
		t.Fatal("unverified inherited workload identity accepted")
	}
}

func TestExternalDestinationCapabilitiesStayIsolated(t *testing.T) {
	_, app, project := fixture()
	k := &kubernetesContext{settings: map[string]any{"application.resourceTrackingMethod": "annotation"}, version: "v1.31.0", apis: []any{"v1"}}
	calls := 0
	k.capabilities = func(_ context.Context, server, name string) (argocd.ClusterCapabilities, error) {
		calls++
		if name == "remote" {
			return argocd.ClusterCapabilities{Server: "https://remote.example", Name: name, Version: "v1.32.0", APIVersions: []string{"example.com/v1/Remote"}}, nil
		}
		if server == "https://other.example" {
			return argocd.ClusterCapabilities{Server: server, Version: "v1.33.0", APIVersions: []string{"other.com/v1/Other"}}, nil
		}
		return argocd.ClusterCapabilities{}, errors.New("denied")
	}
	app.Spec["destination"] = map[string]any{"name": "remote", "namespace": "demo"}
	b, err := k.binding(context.Background(), app)
	if err != nil || b.Request["kubeVersion"] != "v1.32.0" || b.resolvedServer != "https://remote.example" {
		t.Fatal("named destination did not use its own capabilities")
	}
	project["destinations"] = []any{map[string]any{"server": "https://remote.example", "namespace": "demo"}}
	client := &Client{API: fakeAPI(t, func(int) map[string]any { return project }), kubernetes: k, snapshot: Snapshot{Version: "v1alpha1", Instance: "test", ContextID: "automatic", ArgoCDNamespace: "argocd"}, invoke: func(_ context.Context, request map[string]any) (Response, error) {
		if request["kubeVersion"] != "v1.32.0" {
			t.Fatal("wrong cluster capability sent to repo-server")
		}
		return Response{Revision: commit, SourceType: "Helm"}, nil
	}}
	if _, err := client.RenderDesired(context.Background(), app, commit); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("base/head must reuse the captured destination capabilities")
	}
	app.Spec["destination"] = map[string]any{"server": "https://other.example", "namespace": "demo"}
	b, err = k.binding(context.Background(), app)
	if err != nil || b.Request["kubeVersion"] != "v1.33.0" || calls != 2 {
		t.Fatal("different cluster reused prior cache")
	}
	app.Spec["destination"] = map[string]any{"name": "denied", "namespace": "demo"}
	if _, err := k.binding(context.Background(), app); err == nil {
		t.Fatal("missing remote capabilities fell back to local cluster")
	}
	k.clusters = nil
	k.clusterBytes = 16 << 20
	app.Spec["destination"] = map[string]any{"name": "remote", "namespace": "demo"}
	if _, err := k.binding(context.Background(), app); err == nil {
		t.Fatal("unbounded destination capability cache")
	}
}
