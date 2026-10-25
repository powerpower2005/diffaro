package argocd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"diffaro/internal/httpx"
)

func TestClusterCapabilityIdentityAndCompleteness(t *testing.T) {
	for _, tc := range []struct {
		name, returned, status string
		apis                   []string
		denied, bad            bool
	}{
		{"complete", "remote", "Successful", []string{"v1", "apps/v1/Deployment", "v1"}, false, false},
		{"wrong identity", "other", "Successful", []string{"v1"}, false, true},
		{"empty cache", "remote", "Successful", nil, false, true},
		{"unhealthy", "remote", "Failed", []string{"v1"}, false, true},
		{"denied", "remote", "Successful", []string{"v1"}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/clusters/remote" || r.URL.Query().Get("id.type") != "name" || r.Header.Get("Authorization") != "Bearer instance-token" {
					t.Error("wrong cluster request scope")
				}
				if tc.denied {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"name": tc.returned, "server": "https://remote.example", "info": map[string]any{"serverVersion": "v1.32.0", "apiVersions": tc.apis, "connectionState": map[string]any{"status": tc.status}}})
			}))
			defer s.Close()
			c := &Client{Instance: "one", HTTP: httpx.New(s.URL, "instance-token", time.Second, 1<<20)}
			got, err := c.ClusterCapabilities(context.Background(), "", "remote")
			if (err != nil) != tc.bad {
				t.Fatalf("unexpected result: %+v, %v", got, err)
			}
			if err == nil && (got.Version != "v1.32.0" || len(got.APIVersions) != 2 || got.Server != "https://remote.example") {
				t.Fatal("capability provenance lost")
			}
		})
	}
}

func TestClusterServerIdentifierUsesGatewayEncoding(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI != "/api/v1/clusters/https%3A%2F%2Fremote.example%3A6443?id.type=server" {
			t.Errorf("server identifier not encoded for Argo gateway: %s", r.RequestURI)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"server": "https://remote.example:6443", "info": map[string]any{"serverVersion": "v1.32.0", "apiVersions": []string{"v1", "v1/ConfigMap"}, "connectionState": map[string]any{"status": "Successful"}}})
	}))
	defer s.Close()
	c := &Client{HTTP: httpx.New(s.URL, "", time.Second, 1<<20)}
	if _, err := c.ClusterCapabilities(context.Background(), "https://remote.example:6443", ""); err != nil {
		t.Fatal(err)
	}
}
