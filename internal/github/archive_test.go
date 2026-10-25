package github

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/httpx"
)

func sourceTar(t *testing.T, name string, kind byte, data string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tr := tar.NewWriter(gz)
	size := int64(len(data))
	if kind != tar.TypeReg {
		size = 0
	}
	if err := tr.WriteHeader(&tar.Header{Name: name, Typeflag: kind, Mode: 0600, Size: size, Linkname: "../../outside"}); err != nil {
		t.Fatal(err)
	}
	if kind == tar.TypeReg {
		tr.Write([]byte(data))
	}
	tr.Close()
	gz.Close()
	return buf.Bytes()
}

func TestReadArchiveBoundsAndPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  byte
		data  string
		limit int64
		good  bool
	}{{"source/chart/Chart.yaml", tar.TypeReg, "name: chart", 1024, true}, {"source/../../outside", tar.TypeReg, "secret", 1024, false}, {"source/link", tar.TypeSymlink, "", 1024, false}, {"source/large", tar.TypeReg, strings.Repeat("x", 2048), 1024, false}} {
		files, err := ReadArchive(bytes.NewReader(sourceTar(t, tc.name, tc.kind, tc.data)), tc.limit)
		if (err == nil) != tc.good {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tc.good && string(files["chart/Chart.yaml"]) != tc.data {
			t.Fatal(files)
		}
	}
}

// The archive byte bound is render.maxResponseBytes and its failure names
// that setting and the measurement; the fixed entry bound has its own
// message.
func TestArchiveBoundsNameTheirLimit(t *testing.T) {
	// Incompressible content keeps the compressed stream over the bound
	// while the file itself fits it.
	incompressible := make([]byte, 0, 900)
	for block := sha256.Sum256([]byte("fictional")); len(incompressible) < 900; block = sha256.Sum256(block[:]) {
		incompressible = append(incompressible, block[:]...)
	}
	many := func() []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tr := tar.NewWriter(gz)
		for i := 0; i <= MaxArchiveEntries; i++ {
			tr.WriteHeader(&tar.Header{Name: fmt.Sprintf("source/f%d", i), Typeflag: tar.TypeReg, Mode: 0600})
		}
		tr.Close()
		gz.Close()
		return buf.Bytes()
	}
	for _, tc := range []struct {
		name    string
		archive []byte
		limit   int64
		reason  string
		action  string
	}{
		{"expanded bytes", sourceTar(t, "source/large", tar.TypeReg, strings.Repeat("x", 2048)), 1024, "source archive exceeds render.maxResponseBytes: observed at least 2048 bytes; limit 1024 bytes", "increase render.maxResponseBytes above 1024"},
		{"compressed bytes", sourceTar(t, "source/random", tar.TypeReg, string(incompressible[:900])), 1000, "source archive exceeds render.maxResponseBytes: observed at least 1001 bytes; limit 1000 bytes", "increase render.maxResponseBytes above 1000"},
		{"entries", many(), 64 << 20, "source archive has more than 10000 entries", "this entry limit is fixed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadArchive(bytes.NewReader(tc.archive), tc.limit)
			var failure *domain.Failure
			if !errors.As(err, &failure) || failure.Issue.Code != domain.CodeLimitExceeded || failure.Issue.Reason != tc.reason || !strings.HasPrefix(failure.Issue.Action, tc.action) {
				t.Fatalf("got %v, want reason %q and action %q", err, tc.reason, tc.action)
			}
		})
	}
}

func TestArchiveReadOnlyAndRedirectIsolation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer fictional-pat" {
			t.Error("credential or mutation boundary")
		}
		if strings.HasSuffix(r.URL.Path, "/"+strings.Repeat("a", 40)) {
			w.Write(sourceTar(t, "source/Chart.yaml", tar.TypeReg, "name: chart"))
		} else {
			t.Error(r.URL.Path)
		}
	}))
	defer server.Close()
	c := Client{HTTP: httpx.New(server.URL, "fictional-pat", time.Second, 1<<20)}
	if _, _, err := c.Archive(context.Background(), "example/charts", strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("PAT or source request crossed host") }))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer redirect.Close()
	c.HTTP = httpx.New(redirect.URL, "fictional-pat", time.Second, 1<<20)
	if _, _, err := c.Archive(context.Background(), "example/charts", strings.Repeat("a", 40)); err == nil {
		t.Fatal("untrusted redirect accepted")
	}
}

func TestReadGitArchiveGlobalMetadataAndDirectories(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range []*tar.Header{
		{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": strings.Repeat("a", 40)}},
		{Name: "source/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "source/app/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "source/app/Chart.yaml", Typeflag: tar.TypeReg, Mode: 0644, Size: 4},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte("test"))
		}
	}
	tw.Close()
	gz.Close()
	files, err := ReadArchive(&buf, 8192)
	if err != nil || len(files) != 1 || string(files["app/Chart.yaml"]) != "test" {
		t.Fatal(files, err)
	}
}

func linkTar(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tr := tar.NewWriter(gz)
	tr.WriteHeader(&tar.Header{Name: "source/chart/Chart.yaml", Typeflag: tar.TypeReg, Mode: 0600, Size: 11})
	tr.Write([]byte("name: chart"))
	for name, target := range entries {
		if err := tr.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Mode: 0777, Linkname: target}); err != nil {
			t.Fatal(err)
		}
	}
	tr.Close()
	gz.Close()
	return buf.Bytes()
}

// Argo CD accepts symbolic links that stay inside the repository. Source
// archives record them by path; escaping links and chart archives still fail.
func TestSourceArchiveRecordsInsideLinksOnly(t *testing.T) {
	files, links, err := readArchive(bytes.NewReader(linkTar(t, map[string]string{"source/.tools/hooks": "../scripts/hooks", "source/Makefile": "build/Makefile"})), 1024, true)
	if err != nil || string(files["chart/Chart.yaml"]) != "name: chart" || len(links) != 2 {
		t.Fatalf("inside links must be recorded: files=%v links=%v err=%v", files, links, err)
	}
	for _, target := range []string{"../../outside", "/etc/passwd", "a/../../../x"} {
		if _, _, err := readArchive(bytes.NewReader(linkTar(t, map[string]string{"source/link": target})), 1024, true); err == nil {
			t.Fatalf("escaping link %q accepted", target)
		}
	}
	if _, err := ReadArchive(bytes.NewReader(linkTar(t, map[string]string{"source/Makefile": "build/Makefile"})), 1024); err == nil {
		t.Fatal("chart archives must keep rejecting links")
	}
}
