package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// A value masked as a Secret payload stays masked where it reappears in
// another rendered resource, in its field changes and line hunks and in
// every output format; other values stay visible. The
// ConfigMap is compared before the Secret, so masking must happen after
// every comparison of the run.
func TestSecretValueStaysMaskedInOtherResources(t *testing.T) {
	argo := newArgoCDFake(t, argocdFake{token: "fictional-argo-0", apps: func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/applications/api":
			writeJSON(w, testApp())
		case "/api/v1/applications/api/manifests":
			side := "base"
			if r.URL.Query().Get("revision") == headSHA {
				side = "head"
			}
			writeManifests(w, []string{
				fmt.Sprintf(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"creds","namespace":"demo"},"stringData":{"key":"fictional-shared-key-%s"}}`, side),
				fmt.Sprintf(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","namespace":"demo"},"data":{"copy":"fictional-shared-key-%s","plain":"visible-%s"}}`, side, side),
			})
		default:
			http.NotFound(w, r)
		}
	}})
	cfg := configFile(t, gitServer(t).URL, argo.URL)
	for _, format := range []string{"json", "markdown", "pr-markdown", "terminal"} {
		var out, errout bytes.Buffer
		code := Run(context.Background(), []string{"diff", "--config", cfg, "--format", format, "--quiet", "--base", "main", "--head", "feature"}, &out, &errout)
		text := out.String() + errout.String()
		if code != 1 {
			t.Fatalf("%s: exit %d\n%s", format, code, text)
		}
		if strings.Contains(text, "fictional-shared-key") {
			t.Fatalf("%s shows a masked Secret value in another resource:\n%s", format, text)
		}
		if !strings.Contains(text, "visible-head") {
			t.Fatalf("%s hid an ordinary value:\n%s", format, text)
		}
		if format != "json" {
			continue
		}
		var report domain.Report
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		for _, resource := range report.Applications[0].Resources {
			if resource.ID.Kind != "ConfigMap" {
				continue
			}
			changes := map[string]domain.Change{}
			for _, change := range resource.Changes {
				changes[change.Path] = change
			}
			if copied := changes["/data/copy"]; copied.Before != security.Mask || copied.After != security.Mask || !copied.Redacted {
				t.Fatalf("copied Secret value not masked: %+v", copied)
			}
			if plain := changes["/data/plain"]; plain.Redacted || plain.After != "visible-head" {
				t.Fatalf("ordinary value masked: %+v", plain)
			}
			masked := false
			for _, hunk := range resource.LineHunks {
				for _, line := range hunk.Lines {
					masked = masked || strings.Contains(line, "copy: "+security.Mask)
				}
			}
			if len(resource.LineHunks) == 0 || !masked {
				t.Fatalf("line hunks do not mask the copied value: %+v", resource.LineHunks)
			}
		}
	}
}
