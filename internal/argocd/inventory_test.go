package argocd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"diffaro/internal/httpx"
)

func TestInventoryOwnershipAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		limit      int
		wantErr    bool
		owner      string
	}{
		{"controller", `{"items":[{"metadata":{"name":"api","namespace":"argocd","ownerReferences":[{"apiVersion":"argoproj.io/v1alpha1","kind":"ApplicationSet","name":"fleet","uid":"123","controller":true}]},"spec":{}}]}`, 2, false, "fleet"},
		{"label-not-owner", `{"items":[{"metadata":{"name":"api","namespace":"argocd","labels":{"argocd.argoproj.io/application-set-name":"fleet"}},"spec":{}}]}`, 2, false, ""},
		{"foreign-namespace", `{"items":[{"metadata":{"name":"api","namespace":"other"},"spec":{}}]}`, 2, true, ""},
		{"missing-uid", `{"items":[{"metadata":{"name":"api","namespace":"argocd","ownerReferences":[{"apiVersion":"argoproj.io/v1alpha1","kind":"ApplicationSet","name":"fleet","controller":true}]},"spec":{}}]}`, 2, true, ""},
		{"pagination", `{"metadata":{"continue":"next"},"items":[]}`, 2, true, ""},
		{"limit", `{"items":[{"metadata":{"name":"api","namespace":"argocd"},"spec":{}}]}`, 0, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/applications" || r.URL.Query().Get("appNamespace") != "argocd" {
					t.Error("unexpected or mutating request")
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			c := Client{Instance: "test", HTTP: httpx.New(s.URL, "", time.Second, 4096)}
			apps, err := c.ListApplications(context.Background(), "argocd", tc.limit)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if !tc.wantErr && (len(apps) != 1 || apps[0].OwnerName != tc.owner) {
				t.Fatal("ownership lost or inferred from labels")
			}
		})
	}
}

func TestInventoryReportsWhetherApplicationsManageChildren(t *testing.T) {
	body := `{"items":[
		{"metadata":{"name":"leaf","namespace":"argocd"},"spec":{},"status":{"resources":[{"group":"apps","kind":"Deployment"}]}},
		{"metadata":{"name":"parent","namespace":"argocd"},"spec":{},"status":{"resources":[{"group":"argoproj.io","kind":"Application"}]}},
		{"metadata":{"name":"set","namespace":"argocd"},"spec":{},"status":{"resources":[{"group":"argoproj.io","kind":"ApplicationSet"}]}},
		{"metadata":{"name":"empty","namespace":"argocd"},"spec":{},"status":{"resources":[]}},
		{"metadata":{"name":"unknown","namespace":"argocd"},"spec":{}},
		{"metadata":{"name":"synced-empty","namespace":"argocd"},"spec":{"source":{"repoURL":"r","path":"p"},"destination":{"name":"c"}},"status":{"reconciledAt":"2026-01-01T00:00:00Z","sync":{"status":"Synced","comparedTo":{"source":{"repoURL":"r","path":"p"},"destination":{"name":"c"}}}}},
		{"metadata":{"name":"stale-empty","namespace":"argocd"},"spec":{"source":{"repoURL":"r","path":"new"},"destination":{"name":"c"}},"status":{"reconciledAt":"2026-01-01T00:00:00Z","sync":{"status":"Synced","comparedTo":{"source":{"repoURL":"r","path":"p"},"destination":{"name":"c"}}}}},
		{"metadata":{"name":"outofsync-empty","namespace":"argocd"},"spec":{"source":{"repoURL":"r","path":"p"}},"status":{"reconciledAt":"2026-01-01T00:00:00Z","sync":{"status":"OutOfSync","comparedTo":{"source":{"repoURL":"r","path":"p"}}}}},
		{"metadata":{"name":"unreconciled","namespace":"argocd"},"spec":{"source":{"repoURL":"r","path":"p"}},"status":{"sync":{"status":"Synced","comparedTo":{"source":{"repoURL":"r","path":"p"}}}}},
		{"metadata":{"name":"lookalike","namespace":"argocd"},"spec":{},"status":{"resources":[{"group":"example.com","kind":"Application"}]}}
	]}`
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer s.Close()
	c := Client{Instance: "test", HTTP: httpx.New(s.URL, "", time.Second, 1<<20)}
	apps, err := c.ListApplications(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"leaf": "false", "parent": "true", "set": "true", "empty": "false", "unknown": "unknown", "lookalike": "false", "synced-empty": "false", "stale-empty": "unknown", "outofsync-empty": "unknown", "unreconciled": "unknown"}
	for _, app := range apps {
		got := "unknown"
		if app.ManagesApplications != nil {
			got = map[bool]string{true: "true", false: "false"}[*app.ManagesApplications]
		}
		if got != want[app.Application.ID.Name] {
			t.Fatalf("%s manages children=%s, want %s", app.Application.ID.Name, got, want[app.Application.ID.Name])
		}
	}
}
