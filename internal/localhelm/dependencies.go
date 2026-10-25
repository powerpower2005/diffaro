package localhelm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/fetchcache"
	"diffaro/internal/github"
	"diffaro/internal/httpx"

	"github.com/Masterminds/semver/v3"
	"go.yaml.in/yaml/v3"
	chart "helm.sh/helm/v4/pkg/chart/v2"
)

// Only public HTTPS repository indexes and archives are downloaded here.
// No PAT/Argo token is attached, and no Helm plugin is executed.
func (c *Client) publicDownload(ctx context.Context, raw string, limit int64) (data []byte, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, skip("dependency URL requires unsupported credentials or transport")
	}
	// The trace and the skip reason name the public repository host only,
	// never the URL.
	status := 0
	end := domain.TraceHTTP(ctx, "Helm repository", "dependency download", u.Host)
	defer func() { end(status, err) }()
	client := c.publicClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, renderError()
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, errAddressBlocked) {
			// A policy refusal, not a network fault: a retry repeats it.
			return nil, skipWith("public Helm dependency download from "+u.Host+" was refused by policy: the host resolves to a private, loopback, link-local or reserved address", err)
		}
		_, problem := httpx.TransportCause(err)
		return nil, &fetchcache.TransientError{Err: skipWith("public Helm dependency download from "+u.Host+" "+problem, err)}
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, &fetchcache.TransientError{Err: skip(fmt.Sprintf("Helm dependency download from %s returned HTTP %d: the download is unavailable", u.Host, resp.StatusCode))}
	}
	if resp.StatusCode != 200 {
		return nil, skip(fmt.Sprintf("Helm dependency download from %s returned HTTP %d: it requires additional authentication or is unavailable", u.Host, resp.StatusCode))
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, &fetchcache.TransientError{Err: renderError()}
	}
	if int64(len(data)) > limit {
		return nil, renderError()
	}
	return data, nil
}

// publicClient returns this instance's client for public dependency
// downloads, built once so that downloads from one host reuse connections.
func (c *Client) publicClient() *http.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.public == nil {
		c.public = newPublicClient(publicDial, nil)
	}
	return c.public
}

// newPublicClient returns a client that dials with dial, sends no proxy or
// credentials and follows no redirect. roots replaces the system roots in
// tests only.
func newPublicClient(dial func(context.Context, string, string) (net.Conn, error), roots *x509.CertPool) *http.Client {
	transport := &http.Transport{DialContext: dial, IdleConnTimeout: 90 * time.Second}
	if roots != nil {
		transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// errAddressBlocked is the dial guard's refusal of a host that resolves to a
// non-public address.
var errAddressBlocked = errors.New("host resolves to a non-public address")

// reservedPrefixes are non-public IPv4 ranges that the net.IP predicates do
// not cover: "this network", shared address space (carrier-grade NAT, also
// used for pod and overlay networks), IETF protocol assignments and
// benchmarking.
var reservedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
}

// nat64Prefix is the well-known NAT64 prefix; its last four bytes are an IPv4
// address that a NAT64 gateway connects to.
var nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")

// blockedAddress reports whether a dependency download must not connect to
// ip: anything that is not a public unicast address.
func blockedAddress(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	if nat64Prefix.Contains(addr) {
		v4 := addr.As16()
		return blockedAddress(net.IP(v4[12:]))
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return true
	}
	for _, prefix := range reservedPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// publicDial connects only to a public unicast address of the host.
func publicDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	for _, ip := range ips {
		if blockedAddress(ip.IP) {
			return nil, errAddressBlocked
		}
	}
	return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

// prepareDependencies returns files with every dependency of the chart at
// root that is not vendored made available: file:// charts are checked in
// place and locked public chart archives are downloaded under charts/,
// recursively.
func (c *Client) prepareDependencies(ctx context.Context, files map[string][]byte, links []string, root string, depth int) (map[string][]byte, error) {
	if depth > 16 {
		return nil, renderError()
	}
	if linked(links, root) {
		return nil, linkedInput()
	}
	files, err := expandVendoredArchives(files, root, c.Limit)
	if err != nil {
		return nil, err
	}
	ch, err := loadChart(files, root)
	if err != nil {
		return nil, err
	}
	// files can be the caller's map, such as a cached source archive; it is
	// copied once before the first download adds to it.
	copied := false
	for _, dep := range ch.Metadata.Dependencies {
		if dependencyLoaded(ch, dep.Name) {
			continue
		}
		depRoot, local := fileDependencyDir(root, dep.Repository)
		if local {
			if !domain.SafeRelativePath(depRoot) {
				return nil, renderError()
			}
		} else {
			if !copied {
				files, copied = maps.Clone(files), true
			}
			depRoot = path.Join(root, "charts", dep.Name)
			if err = c.downloadDependency(ctx, ch, dep, files, depRoot); err != nil {
				return nil, err
			}
		}
		// The result is files or a copy made below, so a copy stays owned.
		files, err = c.prepareDependencies(ctx, files, links, depRoot, depth+1)
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

// downloadDependency adds the files of dep's locked public chart archive to
// files under depRoot, within the instance's render limit.
func (c *Client) downloadDependency(ctx context.Context, ch *chart.Chart, dep *chart.Dependency, files map[string][]byte, depRoot string) error {
	version, err := lockedVersion(ch, dep)
	if err != nil {
		return err
	}
	data, err := c.dependencyArchive(ctx, dep, version)
	if err != nil {
		return err
	}
	depFiles, err := github.ReadArchive(bytes.NewReader(data), c.Limit)
	if err != nil {
		return err
	}
	var size int64
	for _, data := range files {
		size += int64(len(data))
	}
	for name, data := range depFiles {
		size += int64(len(data))
		if size > c.Limit {
			return renderError()
		}
		files[path.Join(depRoot, name)] = data
	}
	return nil
}

// lockedVersion returns the exact version of dep: the Chart.lock entry when
// the chart has a lock, else the declared version, which must be exact and
// satisfy the declared constraint.
func lockedVersion(ch *chart.Chart, dep *chart.Dependency) (string, error) {
	version := dep.Version
	if ch.Lock != nil {
		found := false
		for _, locked := range ch.Lock.Dependencies {
			if locked.Name == dep.Name && locked.Repository == dep.Repository {
				version = locked.Version
				found = true
				break
			}
		}
		if !found {
			return "", renderError()
		}
	}
	v, err := semver.NewVersion(version)
	if err != nil || (version != v.String() && version != "v"+v.String()) {
		return "", skip("Helm dependency requires a locked exact version")
	}
	constraint, err := semver.NewConstraint(dep.Version)
	if err != nil || !constraint.Check(v) {
		return "", renderError()
	}
	return version, nil
}

// dependencyArchive downloads the archive of dep at version named by its
// repository's index.yaml and verifies the index digest when present.
func (c *Client) dependencyArchive(ctx context.Context, dep *chart.Dependency, version string) ([]byte, error) {
	repo, err := url.Parse(strings.TrimRight(dep.Repository, "/") + "/")
	if err != nil {
		return nil, renderError()
	}
	index, err := c.repositoryIndex(ctx, repo.ResolveReference(&url.URL{Path: "index.yaml"}).String())
	if err != nil {
		return nil, err
	}
	entry := index[indexKey(dep.Name, version)]
	archiveURL, digest := entry.url, entry.digest
	if archiveURL == "" {
		return nil, skip("locked Helm dependency was not found in the public repository index")
	}
	location, err := url.Parse(archiveURL)
	if err != nil {
		return nil, renderError()
	}
	data, err := c.download(ctx, repo.ResolveReference(location).String(), c.Limit)
	if err != nil {
		return nil, err
	}
	if digest != "" {
		sum := sha256.Sum256(data)
		if !strings.EqualFold(digest, hex.EncodeToString(sum[:])) {
			return nil, renderError()
		}
	}
	return data, nil
}

// indexEntry is the first archive URL and digest a repository index lists
// for one chart version.
type indexEntry struct{ url, digest string }

func indexKey(name, version string) string { return name + "\x00" + version }

// repositoryIndex returns the downloaded index.yaml at location parsed into
// the first listed archive of every chart version, so each dependency of
// every render looks it up without parsing the index again. Only parsed
// indexes are kept: every failure is returned as before and retried by the
// next caller, and the index bytes stay in this instance's download snapshot.
func (c *Client) repositoryIndex(ctx context.Context, location string) (map[string]indexEntry, error) {
	c.mu.Lock()
	if c.indexes == nil {
		c.indexes = map[string]*fetchcache.Entry[map[string]indexEntry]{}
	}
	c.mu.Unlock()
	return fetchcache.Load(ctx, &c.mu, c.indexes, location, func(ctx context.Context) (map[string]indexEntry, error) {
		data, err := c.download(ctx, location, c.Limit)
		if err != nil {
			// c.download already keeps its settled failures.
			return nil, &fetchcache.TransientError{Err: err}
		}
		var index struct {
			Entries map[string][]struct {
				Version string
				URLs    []string `yaml:"urls"`
				Digest  string
			}
		}
		if yaml.Unmarshal(data, &index) != nil {
			return nil, &fetchcache.TransientError{Err: renderError()}
		}
		entries := map[string]indexEntry{}
		for name, versions := range index.Entries {
			for _, entry := range versions {
				key := indexKey(name, entry.Version)
				if _, found := entries[key]; !found && len(entry.URLs) > 0 {
					entries[key] = indexEntry{entry.URLs[0], entry.Digest}
				}
			}
		}
		return entries, nil
	}, nil)
}

// Convert vendored packages to bounded memory files before upstream chart
// loading, so its archive loader cannot bypass the configured expansion limit.
// Without vendored packages under root, files is returned as it is.
func expandVendoredArchives(files map[string][]byte, root string, limit int64) (map[string][]byte, error) {
	var size int64
	vendored := false
	prefix := path.Join(root, "charts") + "/"
	for name, data := range files {
		size += int64(len(data))
		vendored = vendored || strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".tgz")
	}
	if size > limit {
		return nil, renderError()
	}
	if !vendored {
		return files, nil
	}
	result := maps.Clone(files)
	for depth := 0; depth < 16; depth++ {
		packages := []string{}
		for name := range result {
			if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".tgz") {
				packages = append(packages, name)
			}
		}
		if len(packages) == 0 {
			return result, nil
		}
		sort.Strings(packages)
		for _, name := range packages {
			entries, err := github.ReadArchive(bytes.NewReader(result[name]), limit)
			if err != nil {
				return nil, err
			}
			var metadata struct {
				Name string `yaml:"name"`
			}
			if yaml.Unmarshal(entries["Chart.yaml"], &metadata) != nil || metadata.Name == "" || strings.ContainsAny(metadata.Name, "/\\:") || metadata.Name == "." || metadata.Name == ".." {
				return nil, renderError()
			}
			size -= int64(len(result[name]))
			delete(result, name)
			for entry, data := range entries {
				target := path.Join(path.Dir(name), metadata.Name, entry)
				if _, exists := result[target]; exists {
					return nil, renderError()
				}
				size += int64(len(data))
				if size > limit {
					return nil, renderError()
				}
				result[target] = data
			}
		}
	}
	return nil, renderError()
}

// Freeze public dependency inputs for equivalent base/head rendering. The
// snapshot is private to this instance and bounded with its source archives.
func (c *Client) download(ctx context.Context, location string, limit int64) ([]byte, error) {
	c.mu.Lock()
	if c.downloads == nil {
		c.downloads = map[string]*fetchcache.Entry[[]byte]{}
	}
	c.mu.Unlock()
	return fetchcache.Load(ctx, &c.mu, c.downloads, location, func(ctx context.Context) ([]byte, error) {
		fetch := c.Download
		if fetch == nil {
			fetch = c.publicDownload
		}
		data, err := fetch(ctx, location, limit)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > limit {
			return nil, renderError()
		}
		if c.charge("download:"+location, int64(len(data))) != nil {
			return nil, renderError()
		}
		return data, nil
	}, nil)
}
