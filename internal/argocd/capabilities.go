package argocd

import (
	"context"
	"net/url"
	"sort"
	"strings"

	"diffaro/internal/domain"
)

type ClusterCapabilities struct {
	Server, Name, Version string
	APIVersions           []string
}

// ClusterCapabilities reads the controller's cached discovery through Argo CD;
// it never connects to the destination or retains its credential configuration.
func (c *Client) ClusterCapabilities(ctx context.Context, server, name string) (ClusterCapabilities, error) {
	var out ClusterCapabilities
	if (server == "") == (name == "") || len(server)+len(name) > 2048 {
		return out, domain.Fail(domain.CodeDestinationInvalid, "context", "provide exactly one cluster server or name", "correct the desired Application destination", nil)
	}
	key := server + "\x00" + name
	c.mu.Lock()
	cached, ok := c.clusters[key]
	c.mu.Unlock()
	if ok {
		return cached, nil
	}
	kind, value := "server", server
	if name != "" {
		kind, value = "name", name
	}
	var response struct {
		Server string `json:"server"`
		Name   string `json:"name"`
		Info   struct {
			ServerVersion   string   `json:"serverVersion"`
			APIVersions     []string `json:"apiVersions"`
			ConnectionState struct {
				Status string `json:"status"`
			} `json:"connectionState"`
		} `json:"info"`
	}
	// Argo CD's HTTP gateway requires ':' to be escaped in server identifiers;
	// url.PathEscape leaves it literal and that route returns HTTP 404.
	segment := strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
	if err := c.get(ctx, "/api/v1/clusters/"+segment, url.Values{"id.type": {kind}}, &response, "destination capability lookup", "/api/v1/clusters/{cluster}", "clusters/get for the requested destination", clusterAccess(kind, value)); err != nil {
		return out, err
	}
	if response.Server == "" || (server != "" && response.Server != server) || (name != "" && response.Name != name) {
		return out, domain.Fail(domain.CodeResponseInvalid, "context", "Argo CD returned a different cluster identity", "verify cluster registration and destination routing", nil)
	}
	if response.Info.ConnectionState.Status != "Successful" || response.Info.ServerVersion == "" || len(response.Info.APIVersions) == 0 || len(response.Info.APIVersions) > 20000 {
		return out, domain.Fail(domain.CodeClusterCapabilitiesUnavailable, "context", "Argo CD cluster discovery is missing, incomplete or unhealthy", "allow clusters/get and wait for controller discovery, or use a trusted contextFile", nil)
	}
	seen := map[string]bool{}
	for _, version := range response.Info.APIVersions {
		if version == "" || len(version) > 600 {
			return out, domain.Fail(domain.CodeResponseInvalid, "context", "Argo CD returned invalid API capabilities", "check the cluster discovery cache", nil)
		}
		seen[version] = true
	}
	out = ClusterCapabilities{Server: response.Server, Name: response.Name, Version: response.Info.ServerVersion, APIVersions: []string{}}
	size := len(out.Server) + len(out.Name) + len(out.Version)
	for version := range seen {
		out.APIVersions = append(out.APIVersions, version)
		size += len(version)
	}
	sort.Strings(out.APIVersions)
	c.keepCapabilities(key, out, size)
	return out, nil
}

// maxCachedCapabilityBytes bounds the capabilities kept for the run; beyond
// it a destination is read again for each caller.
const maxCachedCapabilityBytes = 16 << 20

// keepCapabilities caches a destination's capabilities for every backend of
// this instance. Callers must not modify the returned APIVersions.
func (c *Client) keepCapabilities(key string, capabilities ClusterCapabilities, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.clusters[key]; ok || c.clusterBytes+size > maxCachedCapabilityBytes {
		return
	}
	if c.clusters == nil {
		c.clusters = map[string]ClusterCapabilities{}
	}
	c.clusters[key] = capabilities
	c.clusterBytes += size
}
