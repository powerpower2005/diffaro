package argocd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/httpx"
)

func TestDeniedRequestsDescribeCredentialResourceActionAndScope(t *testing.T) {
	id := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "api"}
	app := domain.Application{ID: id, Spec: map[string]any{"project": "demo"}}
	for _, tc := range []struct {
		name, resource, target, object string
		call                           func(*Client) error
	}{
		{"project check", "projects", "demo", "demo", func(c *Client) error { return c.CheckProject(context.Background(), app) }},
		{"project spec", "projects", "demo", "demo", func(c *Client) error { _, err := c.ProjectSpec(context.Background(), "demo"); return err }},
		{"application lookup", "applications", "namespace=argocd; application=api", "", func(c *Client) error { _, err := c.Application(context.Background(), id); return err }},
		{"inventory", "applications", "selected RBAC-visible Applications; namespace=argocd", "", func(c *Client) error { _, err := c.ListApplications(context.Background(), "argocd", 1000); return err }},
		{"cluster name", "clusters", "name=demo-cluster", "", func(c *Client) error {
			_, err := c.ClusterCapabilities(context.Background(), "", "demo-cluster")
			return err
		}},
		{"cluster server", "clusters", "server=https://cluster.example.invalid", "", func(c *Client) error {
			_, err := c.ClusterCapabilities(context.Background(), "https://cluster.example.invalid", "")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" {
					t.Error("mutation attempted")
				}
				w.WriteHeader(http.StatusForbidden)
			}))
			defer server.Close()
			client := &Client{Instance: "test", HTTP: httpx.New(server.URL, "fictional-private-value", time.Second, 1024)}
			client.HTTP.TokenEnv = "DIFFARO_TEST_ARGO"
			var failure *domain.Failure
			if !errors.As(tc.call(client), &failure) || failure.Issue.RequiredAccess == nil {
				t.Fatal("access requirement was lost")
			}
			access := failure.Issue.RequiredAccess
			if access.System != "argocd-rbac" || access.Instance != "test" || access.CredentialEnv != "DIFFARO_TEST_ARGO" || access.Resource != tc.resource || access.Action != "get" || access.Target != tc.target || access.Object != tc.object || access.Scope == "" || calls.Load() != 1 {
				t.Fatalf("wrong request requirement or extra API request: %+v", access)
			}
			encoded, _ := json.Marshal(access)
			if strings.Contains(string(encoded), "fictional-private-value") {
				t.Fatal("credential value was retained")
			}
		})
	}
}

func TestManifestAndDriftLookupUseKnownApplicationProject(t *testing.T) {
	id := domain.ApplicationID{Instance: "test", Namespace: "apps", Name: "api"}
	app := domain.Application{ID: id, Spec: map[string]any{"project": "demo"}}
	for _, deniedLookup := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if deniedLookup || strings.HasSuffix(r.URL.Path, "/manifests") {
				w.WriteHeader(403)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "api", "namespace": "apps"}, "spec": app.Spec})
		}))
		client := &Client{Instance: "test", HTTP: httpx.New(server.URL, "fictional-value", time.Second, 1024)}
		client.HTTP.TokenEnv = "DIFFARO_TEST_ARGO"
		_, err := client.Render(context.Background(), app, strings.Repeat("a", 40))
		server.Close()
		var failure *domain.Failure
		if !errors.As(err, &failure) || failure.Issue.RequiredAccess == nil {
			t.Fatal("render requirement was lost")
		}
		access := failure.Issue.RequiredAccess
		if access.Resource != "applications" || access.Object != "" || !strings.Contains(access.Target, "project=demo") || !strings.Contains(access.Scope, "demo/apps/api") || access.CredentialEnv != "DIFFARO_TEST_ARGO" {
			t.Fatalf("known project lost or namespace policy guessed: %+v", access)
		}
	}
}

func TestAPIAccessDoesNotInventRBACResource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer server.Close()
	client := &Client{Instance: "test", HTTP: httpx.New(server.URL, "", time.Second, 1024)}
	for _, call := range []func(context.Context) (string, error){client.BackendVersion, client.EffectiveTrackingMethod} {
		_, err := call(context.Background())
		var failure *domain.Failure
		if !errors.As(err, &failure) || failure.Issue.RequiredAccess == nil || failure.Issue.RequiredAccess.System != "argocd-api" || failure.Issue.RequiredAccess.Resource != "" || failure.Issue.RequiredAccess.Object != "" {
			t.Fatal("API endpoint was described as a nonexistent RBAC resource")
		}
	}
}

func TestClusterAccessDoesNotRetainURLCredentials(t *testing.T) {
	for _, value := range []string{"https://user:fictional-private-value@cluster.example.invalid", "https://cluster.example.invalid?credential=fictional-private-value", "https://cluster.example.invalid#fictional-private-value"} {
		access := clusterAccess("server", value)
		if strings.Contains(access.Target, "fictional-private-value") || access.Object != "" {
			t.Fatal("unsafe or speculative cluster object")
		}
	}
}

// Without a project, Argo CD answers a missing Application with 403. A denied
// unscoped lookup names that possibility and the profile fields to check; a project-scoped read and the AppProject read stay plain
// denials.
func TestUnscopedApplicationDenialNamesMissingApplication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	client := &Client{Instance: "test", HTTP: httpx.New(server.URL, "fictional-value", time.Second, 1024)}
	id := domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "apii"}
	issue := func(err error) domain.Issue {
		var failure *domain.Failure
		if !errors.As(err, &failure) || failure.Issue.Code != domain.CodeAccessDenied || failure.Issue.ReasonCode != domain.ReasonRequestForbidden {
			t.Fatalf("not a forbidden denial: %v", err)
		}
		return failure.Issue
	}
	_, err := client.Application(context.Background(), id)
	unscoped := issue(err)
	if !strings.Contains(unscoped.Reason, "also answers 403 when no Application of this name exists") || !strings.Contains(unscoped.Action, "applications[].name and namespace") || !strings.Contains(unscoped.Action, "init --endpoint URL") {
		t.Fatalf("missing Application not named: %+v", unscoped)
	}
	_, err = client.application(context.Background(), id, "demo")
	scoped := issue(err)
	err = client.CheckProject(context.Background(), domain.Application{ID: id, Spec: map[string]any{"project": "demo"}})
	project := issue(err)
	for _, denied := range []domain.Issue{scoped, project} {
		if strings.Contains(denied.Reason+denied.Action, "also answers 403") {
			t.Fatalf("scoped read named a missing Application: %+v", denied)
		}
	}
}
