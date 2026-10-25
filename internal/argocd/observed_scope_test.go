package argocd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/httpx"
)

func TestObservedScopes(t *testing.T) {
	got := observedScopes([]statusResource{
		{Group: "iam.services.k8s.aws", Kind: "Role", Namespace: "team"},
		{Group: "iam.services.k8s.aws", Kind: "Role", Namespace: "other"},
		{Group: "example.com", Kind: "Global"},
		{Group: "example.com", Kind: "Mixed", Namespace: "team"},
		{Group: "example.com", Kind: "Mixed"},
		{Group: "example.com"},
	})
	want := map[string]bool{"iam.services.k8s.aws/Role": false, "example.com/Global": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if observedScopes(nil) != nil {
		t.Fatal("empty status produced scopes")
	}
}

func TestApplicationLookupReadsObservedScopes(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{"name": "iam", "namespace": "argocd"},
			"spec":     map[string]any{"project": "demo"},
			"status":   map[string]any{"resources": []any{map[string]any{"group": "iam.services.k8s.aws", "kind": "Policy", "namespace": "team", "name": "p"}}},
		})
	}))
	defer s.Close()
	c := Client{Instance: "test", HTTP: httpx.New(s.URL, "", time.Second, 1<<20)}
	app, err := c.Application(context.Background(), domain.ApplicationID{Instance: "test", Namespace: "argocd", Name: "iam"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(app.ObservedScopes, map[string]bool{"iam.services.k8s.aws/Policy": false}) {
		t.Fatalf("got %v", app.ObservedScopes)
	}
}

func TestInventoryReadsObservedScopes(t *testing.T) {
	body := `{"items":[{"metadata":{"name":"iam","namespace":"argocd"},"spec":{},"status":{"resources":[{"group":"iam.services.k8s.aws","kind":"Role","namespace":"team"}]}}]}`
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer s.Close()
	c := Client{Instance: "test", HTTP: httpx.New(s.URL, "", time.Second, 1<<20)}
	apps, err := c.ListApplications(context.Background(), "", 10)
	if err != nil || len(apps) != 1 || !reflect.DeepEqual(apps[0].Application.ObservedScopes, map[string]bool{"iam.services.k8s.aws/Role": false}) {
		t.Fatalf("inventory scopes lost: %v %+v", err, apps)
	}
}
