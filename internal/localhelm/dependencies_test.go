package localhelm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/fetchcache"
)

// The parsed repository index serves every dependency of every render of
// one instance, while each instance still downloads and charges the index
// against its own snapshot limit.
func TestRepositoryIndexIsReadOncePerInstance(t *testing.T) {
	files, download := largeIndexDependencies(t, 20)
	var indexReads atomic.Int32
	counting := func(ctx context.Context, location string, limit int64) ([]byte, error) {
		if strings.HasSuffix(location, "index.yaml") {
			indexReads.Add(1)
		}
		return download(ctx, location, limit)
	}
	east := &Client{Limit: 64 << 20, Download: counting}
	for i := 0; i < 3; i++ {
		prepared, err := east.prepareDependencies(context.Background(), files, nil, "app", 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"app/charts/chart0/Chart.yaml", "app/charts/chart19/Chart.yaml"} {
			if prepared[name] == nil {
				t.Fatal("locked dependency archive was not added", name)
			}
		}
	}
	if indexReads.Load() != 1 || len(east.indexes) != 1 {
		t.Fatalf("one instance read the index %d times into %d parsed indexes", indexReads.Load(), len(east.indexes))
	}
	// Another instance with a small snapshot limit is charged for its own
	// index download instead of reusing the parsed index of the first one.
	west := &Client{Limit: 64 << 20, SnapshotLimit: 1024, Download: counting}
	_, err := west.prepareDependencies(context.Background(), files, nil, "app", 0)
	var failure *domain.Failure
	if !errors.As(err, &failure) || failure.Issue.Code != domain.CodeLocalRenderFailed {
		t.Fatal("instance snapshot limit did not bound its own index download", err)
	}
	if indexReads.Load() != 2 {
		t.Fatal("instances shared an index download", indexReads.Load())
	}
}

// Only parsed indexes are kept: an interrupted index download is retried
// by the next render and a malformed index fails every render the same way.
func TestRepositoryIndexFailuresAreNotKept(t *testing.T) {
	files, download := largeIndexDependencies(t, 1)
	var indexReads atomic.Int32
	c := &Client{Limit: 64 << 20, Download: func(ctx context.Context, location string, limit int64) ([]byte, error) {
		if strings.HasSuffix(location, "index.yaml") && indexReads.Add(1) == 1 {
			return nil, &fetchcache.TransientError{Err: skip("public Helm dependency download is unavailable")}
		}
		return download(ctx, location, limit)
	}}
	var skipped *domain.RenderSkipped
	if _, err := c.prepareDependencies(context.Background(), files, nil, "app", 0); !errors.As(err, &skipped) {
		t.Fatal("interrupted index download was not reported", err)
	}
	if _, err := c.prepareDependencies(context.Background(), files, nil, "app", 0); err != nil || indexReads.Load() != 2 {
		t.Fatal("interrupted index download was kept instead of retried", err, indexReads.Load())
	}

	indexReads.Store(0)
	malformed := &Client{Limit: 64 << 20, Download: func(ctx context.Context, location string, limit int64) ([]byte, error) {
		if strings.HasSuffix(location, "index.yaml") {
			indexReads.Add(1)
			return []byte("entries: ["), nil
		}
		return download(ctx, location, limit)
	}}
	for i := 0; i < 2; i++ {
		_, err := malformed.prepareDependencies(context.Background(), files, nil, "app", 0)
		var failure *domain.Failure
		if !errors.As(err, &failure) || failure.Issue.ReasonCode != domain.ReasonInputInvalid {
			t.Fatal("malformed index was not reported as invalid input", err)
		}
	}
	if indexReads.Load() != 1 || len(malformed.indexes) != 0 {
		t.Fatal("malformed index bytes were downloaded again or parsed index kept", indexReads.Load(), len(malformed.indexes))
	}
}

// The source files passed in can be a run-cached archive shared by every
// render of the revision; downloaded dependencies go to a copy.
func TestPrepareDependenciesLeavesCallerFilesUnchanged(t *testing.T) {
	files, download := largeIndexDependencies(t, 2)
	files["app/Chart.yaml"] = append(files["app/Chart.yaml"], []byte("  - name: local\n    version: 0.1.0\n    repository: file://../local\n")...)
	files["local/Chart.yaml"] = []byte("apiVersion: v2\nname: local\nversion: 0.1.0\n")
	before := len(files)
	c := &Client{Limit: 64 << 20, Download: download}
	prepared, err := c.prepareDependencies(context.Background(), files, nil, "app", 0)
	if err != nil {
		t.Fatal(err)
	}
	if prepared["app/charts/chart1/Chart.yaml"] == nil || len(files) != before || files["app/charts/chart1/Chart.yaml"] != nil {
		t.Fatalf("downloads were added to the caller's files: before=%d after=%d", before, len(files))
	}
}

// Downloads of one instance share one guarded client, so the index and the
// archives of one repository host reuse a connection.
func TestPublicDownloadsReuseConnections(t *testing.T) {
	files, download := largeIndexDependencies(t, 2)
	var connections, requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		data, err := download(r.Context(), "https://charts.example.invalid"+r.URL.Path, 64<<20)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	files["app/Chart.yaml"] = bytes.ReplaceAll(files["app/Chart.yaml"], []byte("https://charts.example.invalid"), []byte(server.URL))
	// The test server listens on loopback, which the production dial refuses.
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	c := &Client{Limit: 64 << 20, public: newPublicClient(dial, server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs)}
	prepared, err := c.prepareDependencies(context.Background(), files, nil, "app", 0)
	if err != nil || prepared["app/charts/chart1/Chart.yaml"] == nil {
		t.Fatal("public dependencies were not downloaded", err)
	}
	if requests.Load() != 3 || connections.Load() != 1 {
		t.Fatalf("%d downloads opened %d connections", requests.Load(), connections.Load())
	}
}

// The shared client keeps the dial guard: private, loopback and link-local
// addresses are refused before connecting, even to a listening server.
func TestPublicDialRefusesNonPublicAddresses(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, address := range []string{listener.Addr().String(), "10.0.0.1:443", "169.254.169.254:443", "[::1]:443", "100.64.0.1:443"} {
		conn, err := publicDial(context.Background(), "tcp", address)
		if conn != nil {
			conn.Close()
		}
		if !errors.Is(err, errAddressBlocked) {
			t.Fatal("non-public address was not refused by the guard", address, err)
		}
	}
}

// Dependency downloads connect only to public unicast addresses: reserved
// IPv4 ranges outside the net.IP predicates and NAT64 addresses embedding a
// refused IPv4 address are refused too.
func TestBlockedAddressCoversReservedRanges(t *testing.T) {
	for address, blocked := range map[string]bool{
		"10.0.0.1":             true,
		"127.0.0.1":            true,
		"169.254.169.254":      true,
		"fd00:ec2::254":        true,
		"::1":                  true,
		"0.1.2.3":              true, // this network
		"100.64.0.1":           true, // shared address space
		"100.100.100.200":      true,
		"100.127.255.254":      true,
		"192.0.0.8":            true, // IETF protocol assignments
		"198.18.0.1":           true, // benchmarking
		"198.19.255.254":       true,
		"64:ff9b::a00:1":       true, // NAT64 of 10.0.0.1
		"64:ff9b::6440:1":      true, // NAT64 of 100.64.0.1
		"64:ff9b::7f00:1":      true, // NAT64 of 127.0.0.1
		"::ffff:100.64.0.1":    true, // IPv4-mapped
		"100.63.255.255":       false,
		"100.128.0.1":          false,
		"198.17.255.255":       false,
		"198.20.0.1":           false,
		"192.0.1.1":            false,
		"203.0.114.1":          false,
		"64:ff9b::cb00:7201":   false, // NAT64 of public 203.0.114.1
		"2001:db8:ffff::1":     false,
		"2606:4700:4700::1111": false,
		"::ffff:203.0.114.1":   false,
		"64:ff9b:1::a00:1":     false, // local-use NAT64 prefix is not the well-known one
		"64:ff9b::ffff:a00:1":  false, // outside the /96: not an embedded IPv4 address
	} {
		if got := blockedAddress(net.ParseIP(address)); got != blocked {
			t.Errorf("%s: blocked=%v, want %v", address, got, blocked)
		}
	}
}

// A failed public dependency download names the repository host and its
// cause: a policy refusal is not reported as a network fault, a transport
// failure states its cause, and a status names its number.
func TestPublicDownloadFailureNamesHostAndCause(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/busy") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	server.StartTLS()
	defer server.Close()
	roots := server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	// The test server's certificate is valid for example.com.
	toServer := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	dnsFailure := func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "charts.example.invalid", IsNotFound: true}
	}
	for _, tc := range []struct {
		name      string
		client    *http.Client
		url       string
		transient bool
		want      string
	}{
		{"policy", newPublicClient(publicDial, nil), "https://127.0.0.1:1/index.yaml", false, "public Helm dependency download from 127.0.0.1:1 was refused by policy: the host resolves to a private, loopback, link-local or reserved address"},
		{"dns", newPublicClient(dnsFailure, nil), "https://charts.example.invalid/index.yaml", true, "public Helm dependency download from charts.example.invalid failed: the host name could not be resolved"},
		{"not found", newPublicClient(toServer, roots), "https://example.com/index.yaml", false, "Helm dependency download from example.com returned HTTP 404"},
		{"unavailable", newPublicClient(toServer, roots), "https://example.com/busy", true, "Helm dependency download from example.com returned HTTP 503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{public: tc.client}
			_, err := c.publicDownload(context.Background(), tc.url, 1<<20)
			var skipped *domain.RenderSkipped
			var transient *fetchcache.TransientError
			if !errors.As(err, &skipped) || !strings.Contains(skipped.Reason, tc.want) {
				t.Fatalf("skip does not name the host and cause %q: %v", tc.want, err)
			}
			if errors.As(err, &transient) != tc.transient {
				t.Fatalf("transient=%v, want %v: %v", !tc.transient, tc.transient, err)
			}
			if strings.Contains(skipped.Reason, "/index.yaml") || strings.Contains(skipped.Reason, "/busy") {
				t.Fatalf("skip reason names the URL path: %q", skipped.Reason)
			}
		})
	}
}

// largeIndexDependencies returns a chart that declares count public
// dependencies from one repository whose index.yaml lists many charts and
// versions, and a Download serving that index and the dependency archives.
func largeIndexDependencies(tb testing.TB, count int) (map[string][]byte, func(context.Context, string, int64) ([]byte, error)) {
	tb.Helper()
	var chartYAML, index strings.Builder
	chartYAML.WriteString("apiVersion: v2\nname: app\nversion: 0.1.0\ndependencies:\n")
	index.WriteString("apiVersion: v1\nentries:\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&index, "  chart%d:\n", i)
		for v := 0; v < 20; v++ {
			fmt.Fprintf(&index, "    - name: chart%d\n      version: 1.%d.0\n      description: a fictional chart used to size the repository index\n      urls: [chart%d-1.%d.0.tgz]\n", i, v, i, v)
		}
	}
	archives := map[string][]byte{}
	for i := 0; i < count; i++ {
		fmt.Fprintf(&chartYAML, "  - name: chart%d\n    version: 1.3.0\n    repository: https://charts.example.invalid\n", i)
		var data bytes.Buffer
		gz := gzip.NewWriter(&data)
		tw := tar.NewWriter(gz)
		text := fmt.Sprintf("apiVersion: v2\nname: chart%d\nversion: 1.3.0\n", i)
		if err := tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("chart%d/Chart.yaml", i), Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(text))}); err != nil {
			tb.Fatal(err)
		}
		tw.Write([]byte(text))
		tw.Close()
		gz.Close()
		archives[fmt.Sprintf("https://charts.example.invalid/chart%d-1.3.0.tgz", i)] = data.Bytes()
	}
	indexData := []byte(index.String())
	files := map[string][]byte{"app/Chart.yaml": []byte(chartYAML.String()), "app/values.yaml": []byte("replicas: 1\n")}
	return files, func(_ context.Context, location string, _ int64) ([]byte, error) {
		if strings.HasSuffix(location, "index.yaml") {
			return indexData, nil
		}
		if data, ok := archives[location]; ok {
			return data, nil
		}
		return nil, skip("unexpected fixture download")
	}
}
