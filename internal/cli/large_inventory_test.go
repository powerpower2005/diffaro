package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

func TestInitLargeInventoryAndPersistedLimits(t *testing.T) {
	items := make([]any, 256)
	for i := range items {
		app := testApp()
		app["metadata"].(map[string]any)["name"] = fmt.Sprintf("app-%04d", i)
		app["status"] = map[string]any{"fixturePadding": strings.Repeat("x", 40<<10)}
		items[i] = app
	}
	payload, err := json.Marshal(map[string]any{"items": items})
	if err != nil || len(payload) <= 8<<20 {
		t.Fatal("fixture must exceed the former 8 MiB API limit")
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/applications" {
			t.Error("unexpected or mutating inventory request")
		}
		_, _ = w.Write(payload)
	}))
	server.Listener = bufferRetryListener{server.Listener}
	server.Start()
	defer server.Close()
	for _, tc := range []struct {
		name         string
		flags        []string
		code         int
		applications int
		bytes        int64
		snapshot     int64
		diagnostic   string
	}{
		{"defaults", nil, 0, 1000, 64 << 20, 0, ""},
		{"custom", []string{"--max-applications", "2000", "--max-response-bytes", "16777216", "--max-source-snapshot-bytes", "134217728"}, 0, 2000, 16 << 20, 128 << 20, ""},
		{"small response", []string{"--max-response-bytes", "8388608"}, 6, 0, 0, 0, "observed at least 8388609 bytes; limit 8388608 bytes"},
		{"small inventory", []string{"--max-applications", "250"}, 6, 0, 0, 0, "observed 256 Applications; limit 250 Applications"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "profile.yaml")
			args := append([]string{"init", "--endpoint", server.URL, "--pr", "https://github.com/example/charts/pull/12", "--config", path}, tc.flags...)
			var output, diagnostic bytes.Buffer
			if code := Run(context.Background(), args, &output, &diagnostic); code != tc.code {
				t.Fatalf("exit=%d: %s", code, diagnostic.String())
			}
			if tc.code == 0 {
				configuration, err := profile.Load(path)
				if err != nil || configuration.Render.MaxApplications != tc.applications || configuration.Render.MaxResponseBytes != tc.bytes || configuration.Render.MaxSourceSnapshotBytes != tc.snapshot || configuration.Render.MaxDocuments != 10000 {
					t.Fatal("initial connection and generated profile limits differ", err)
				}
				data, _ := os.ReadFile(path)
				assertNoSecrets(t, "generated profile with inventory spec or values", string(data), "fictional-", "fixturePadding")
			} else {
				if !strings.Contains(diagnostic.String(), tc.diagnostic) || !strings.Contains(diagnostic.String(), "--max-") {
					t.Fatalf("missing measured limit and init recovery: %s", diagnostic.String())
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("failed init created a profile")
				}
			}
		})
	}
}

// bufferRetryListener serves a fake backend whose connections retry the rest
// of a write that the kernel rejected with ENOBUFS. macOS returns it for
// large loopback writes when socket buffers run out under load; net/http then
// drops the response and the client reads a truncated body. The payload and
// the HTTP path the client reads are unchanged.
type bufferRetryListener struct{ net.Listener }

func (l bufferRetryListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return bufferRetryConn{c}, nil
}

type bufferRetryConn struct{ net.Conn }

func (c bufferRetryConn) Write(p []byte) (int, error) {
	written := 0
	for {
		n, err := c.Conn.Write(p[written:])
		written += n
		if !errors.Is(err, syscall.ENOBUFS) {
			return written, err
		}
		time.Sleep(time.Millisecond)
	}
}

func TestInitRejectsUnboundedLimitsBeforeNetwork(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(500) }))
	defer server.Close()
	for _, flags := range [][]string{{"--max-applications", "0"}, {"--max-applications", "10001"}, {"--max-response-bytes", "0"}, {"--max-response-bytes", "67108865"}} {
		var output, diagnostic bytes.Buffer
		args := append([]string{"init", "--endpoint", server.URL, "--pr", "https://github.com/example/charts/pull/12"}, flags...)
		if Run(context.Background(), args, &output, &diagnostic) != 4 || !strings.Contains(diagnostic.String(), "render limits") {
			t.Fatal("invalid sizing was not rejected")
		}
	}
	for _, flags := range [][]string{{"--max-source-snapshot-bytes", "1048576"}, {"--max-source-snapshot-bytes", "536870913"}} {
		var output, diagnostic bytes.Buffer
		args := append([]string{"init", "--endpoint", server.URL, "--pr", "https://github.com/example/charts/pull/12"}, flags...)
		if Run(context.Background(), args, &output, &diagnostic) != 4 || !strings.Contains(diagnostic.String(), "maxSourceSnapshotBytes") {
			t.Fatal("invalid source snapshot sizing was not rejected")
		}
	}
	if requests != 0 {
		t.Fatal("invalid sizing reached the backend")
	}
}

func TestResponseLimitKeepsIndependentInstanceAndMeasuredFailure(t *testing.T) {
	g := gitServer(t)
	backend, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/applications/api/manifests" {
			_ = json.NewEncoder(w).Encode(map[string]any{"manifests": []string{strings.Repeat("fictional-sensitive-manifest", 100)}})
			return
		}
		backend.Config.Handler.ServeHTTP(w, r)
	}))
	defer large.Close()
	good, _ := argoServer(t, "fictional-argo-1", backendOptions{})
	config := configFile(t, g.URL, large.URL, good.URL)
	appendProfile(t, config, "render:\n  maxResponseBytes: 1024\n")
	var output, diagnostic bytes.Buffer
	if Run(context.Background(), []string{"diff", "--config", config, "--base", "main", "--head", "feature", "--format", "json", "--quiet"}, &output, &diagnostic) != 3 {
		t.Fatal("size failure hid the independent successful branch")
	}
	var report domain.Report
	if json.Unmarshal(output.Bytes(), &report) != nil || report.Status != "partial_failure" || len(report.Applications) != 2 || report.Applications[0].Execution != "failed" || report.Applications[1].Execution != "complete" {
		t.Fatal("incorrect partial comparison")
	}
	for _, text := range []string{"target manifest rendering", "render.maxResponseBytes", "observed at least 1025 bytes; limit 1024 bytes", "full size is unknown"} {
		if !strings.Contains(diagnostic.String(), text) || !bytes.Contains(output.Bytes(), []byte(text)) {
			t.Fatalf("missing measured diagnostic %s: %s", text, diagnostic.String())
		}
	}
	assertNoSecrets(t, "size failure report", output.String(), "fictional-sensitive-manifest")
	assertNoSecrets(t, "size failure diagnostic", diagnostic.String(), "fictional-")
}
