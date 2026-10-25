package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCLIInitAndPRDiscoveryWithExternalValuesSource(t *testing.T) {
	g := gitServer(t)
	application := testApp()
	spec := application["spec"].(map[string]any)
	chart := spec["source"].(map[string]any)
	chart["repoURL"], chart["targetRevision"] = "https://github.com/example/library.git", baseSHA
	chart["helm"].(map[string]any)["valueFiles"] = []any{"$values/prod.yaml"}
	spec["sources"] = []any{chart, map[string]any{"repoURL": fakeRepo, "targetRevision": "main", "ref": "values"}}
	delete(spec, "source")
	server := readOnlyFake(t, "Argo CD", "fictional-argo-0", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"Version":"v3.5.3"}`)
		case "/api/v1/applications":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{application}})
		case "/api/v1/applications/api":
			_ = json.NewEncoder(w).Encode(application)
		case "/api/v1/projects/demo":
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "demo"}})
		case "/api/v1/applications/api/manifests":
			q := r.URL.Query()
			if !reflect.DeepEqual(q["sourcePositions"], []string{"1", "2"}) || len(q["revisions"]) != 2 || q["revisions"][0] != baseSHA {
				t.Error("all source revisions must be sent in order")
				http.Error(w, "bad source plan", 400)
				return
			}
			replicas := 3
			if q["revisions"][1] == headSHA {
				replicas = 1
			} else if q["revisions"][1] != ancestorSHA {
				t.Error("PR merge base was not used")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"manifests": []string{fmt.Sprintf(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api"},"spec":{"replicas":%d}}`, replicas)}})
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	// Establish separate fixture credentials without relying on a prewritten profile.
	_ = configFile(t, g.URL, server.URL)
	config := filepath.Join(t.TempDir(), "multi.yaml")
	var out, diagnostic bytes.Buffer
	args := []string{"init", "--endpoint", server.URL, "--instance", "instance-0", "--pr", "https://github.com/example/charts/pull/12", "--config", config, "--single-instance", "--git-api", g.URL, "--token-env", "DIFFARO_TEST_ARGO_0", "--git-token-env", "DIFFARO_TEST_GITHUB"}
	if code := Run(context.Background(), args, &out, &diagnostic); code != 0 {
		t.Fatalf("init failed: %d %s", code, diagnostic.String())
	}
	report, code := runJSON(t, config, "--pr", "https://github.com/example/charts/pull/12")
	if code != 1 || report.Status != "success" || len(report.Applications) != 1 || report.Revisions.Base != ancestorSHA {
		t.Fatalf("multi-source PR failed: %d %+v", code, report)
	}
	app := report.Applications[0]
	if app.BaseTarget.Sources[1].Revision != ancestorSHA || app.HeadTarget.Sources[1].Revision != headSHA || app.HeadTarget.Sources[0].Revision != baseSHA {
		t.Fatal("source revision provenance lost")
	}
	if app.HeadTarget.Sources[0].Inputs.ReleaseName != "api-test" || !app.HeadTarget.Sources[0].Inputs.ValuesObject {
		t.Fatal("Helm context summary lost")
	}
	if len(app.Resources) != 1 || app.Resources[0].Changes[0].Path != "/spec/replicas" {
		t.Fatal("rendered target change missing")
	}
	data, _ := json.Marshal(report)
	assertNoSecrets(t, "report with source inputs", string(data), "fictional-object-value")
}
