package profile

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"diffaro/internal/domain"
	"diffaro/internal/policy"
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func ValidEndpoint(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")
}

func invalid(s string) error { return fmt.Errorf("invalid configuration: %s", s) }

// entry names a list entry in a validation message, with its id only when the
// id is a safe identifier. Messages never echo URLs, paths or tokens.
func entry(list string, i int, id string) string {
	s := fmt.Sprintf("%s[%d]", list, i)
	if identifier.MatchString(id) {
		s += " (id=" + id + ")"
	}
	return s
}

// tokenProblem names the failing credential field of a token/tokenEnv pair,
// or returns "" when both are valid.
func tokenProblem(token, tokenEnv string) string {
	if tokenEnv != "" && !envName.MatchString(tokenEnv) {
		return "tokenEnv must be an environment variable name"
	}
	if !ValidToken(token) {
		return "token must be at most 65536 printable characters without spaces"
	}
	return ""
}

func (c Config) Validate() error {
	if c.Version != "v1alpha1" {
		return invalid("version must be v1alpha1")
	}
	if err := c.Git.validate(); err != nil {
		return err
	}
	if err := c.Render.Validate(); err != nil {
		return err
	}
	instances := map[string]bool{}
	for i, instance := range c.Instances {
		if err := instance.validate(i, instances); err != nil {
			return err
		}
		instances[instance.ID] = true
	}
	if err := c.validateDiscovery(); err != nil {
		return err
	}
	if err := c.validateSelections(instances); err != nil {
		return err
	}
	for i, p := range c.SensitivePaths {
		if !strings.HasPrefix(p, "/") || strings.Contains(strings.NewReplacer("~0", "", "~1", "").Replace(p), "~") {
			return invalid(fmt.Sprintf("sensitivePaths must be absolute JSON pointers: sensitivePaths[%d]", i))
		}
	}
	if err := c.Routing.validate(instances); err != nil {
		return err
	}
	if err := c.Run.validate(); err != nil {
		return err
	}
	if err := policy.Validate(c.Policies); err != nil {
		return invalid(err.Error())
	}
	for i, rule := range c.Policies {
		if rule.Match.Instance != "" && !instances[rule.Match.Instance] {
			return invalid(fmt.Sprintf("policy selector references an unconfigured instance: policies[%d] match.instance", i))
		}
	}
	return nil
}

func (g Git) validate() error {
	if !ValidEndpoint(g.APIURL) {
		return invalid("git.apiURL must be HTTPS (HTTP allowed only for loopback) without credentials, query or fragment")
	}
	parts := strings.Split(g.Repository, "/")
	if len(parts) != 2 || !identifier.MatchString(parts[0]) || !identifier.MatchString(parts[1]) {
		return invalid("git.repository must be owner/repository")
	}
	if !ValidEndpoint(g.RepositoryURL) {
		return invalid("git.repositoryURL, token or tokenEnv is invalid: git.repositoryURL must be HTTPS (HTTP allowed only for loopback) without credentials, query or fragment")
	}
	if problem := tokenProblem(g.Token, g.TokenEnv); problem != "" {
		return invalid("git.repositoryURL, token or tokenEnv is invalid: git." + problem)
	}
	repoURL, _ := url.Parse(g.RepositoryURL)
	if strings.TrimSuffix(strings.Trim(repoURL.Path, "/"), ".git") != g.Repository {
		return invalid("git.repositoryURL must identify the configured owner/repository")
	}
	if len(g.RepositoryAliases) > 8 {
		return invalid("git.repositoryAliases allows at most 8 URLs")
	}
	for i, alias := range g.RepositoryAliases {
		aliasURL, err := url.Parse(alias)
		problem := ""
		switch {
		case !ValidEndpoint(alias) || err != nil:
			problem = "must be HTTPS (HTTP allowed only for loopback) without credentials, query or fragment"
		case strings.TrimSuffix(strings.Trim(aliasURL.Path, "/"), ".git") != g.Repository:
			problem = "must identify the configured owner/repository"
		case SameRepositoryAny(alias, g.RepositoryURL, g.RepositoryAliases[:i]):
			problem = "repeats repositoryURL or an earlier alias"
		}
		if problem != "" {
			return invalid(fmt.Sprintf("git.repositoryAliases must be unique HTTPS URLs of the configured owner/repository: git.repositoryAliases[%d] %s", i, problem))
		}
	}
	if len(g.MirrorHosts) > 8 {
		return invalid("git.mirrorHosts allows at most 8 hosts")
	}
	mirrors := map[string]bool{strings.ToLower(repoURL.Host): true}
	for i, host := range g.MirrorHosts {
		mirror, err := url.Parse(host)
		problem := ""
		switch {
		case err != nil || mirror.Scheme != "https" || !ValidEndpoint(host):
			problem = "must be an HTTPS URL without credentials, query or fragment"
		case strings.Trim(mirror.Path, "/") != "":
			problem = "must not have a path"
		case mirrors[strings.ToLower(mirror.Host)]:
			problem = "repeats the repositoryURL host or an earlier mirror host"
		}
		if problem != "" {
			return invalid(fmt.Sprintf("git.mirrorHosts must be unique HTTPS hosts without a path, other than the repositoryURL host: git.mirrorHosts[%d] %s", i, problem))
		}
		mirrors[strings.ToLower(mirror.Host)] = true
	}
	if len(g.LocalRepositories) > MaxLocalRepositories {
		return invalid(fmt.Sprintf("git.localRepositories allows at most %d repositories", MaxLocalRepositories))
	}
	names := map[string]bool{}
	for i, local := range g.LocalRepositories {
		parts := strings.Split(local.Name, "/")
		switch {
		case len(parts) != 2 || !identifier.MatchString(parts[0]) || !identifier.MatchString(parts[1]):
			return invalid(fmt.Sprintf("git.localRepositories[%d].name must be owner/repository", i))
		case names[strings.ToLower(local.Name)]:
			return invalid(fmt.Sprintf("git.localRepositories[%d].name repeats an earlier repository", i))
		case !filepath.IsAbs(local.Path) || filepath.Clean(local.Path) != local.Path || strings.ContainsAny(local.Path, "\x00\r\n"):
			return invalid(fmt.Sprintf("git.localRepositories[%d].path must be a clean absolute directory path", i))
		}
		names[strings.ToLower(local.Name)] = true
	}
	if g.GitExecutable != "" && (!filepath.IsAbs(g.GitExecutable) || strings.ContainsAny(g.GitExecutable, "\x00\r\n")) {
		return invalid("git.gitExecutable must be an absolute path")
	}
	return nil
}

// validate checks instances[i]; seen holds the IDs of earlier instances.
func (i Instance) validate(index int, seen map[string]bool) error {
	name := entry("instances", index, i.ID)
	problem := ""
	switch {
	case !identifier.MatchString(i.ID):
		problem = "id must be a non-empty identifier of letters, digits, '.', '_' or '-'"
	case seen[i.ID]:
		problem = "id repeats an earlier instance"
	case !ValidEndpoint(i.Endpoint):
		problem = "endpoint must be HTTPS (HTTP allowed only for loopback) without credentials, query or fragment"
	default:
		problem = tokenProblem(i.Token, i.TokenEnv)
	}
	if problem != "" {
		return invalid("instances must have unique IDs, valid endpoints, tokens and optional tokenEnv names: " + name + ": " + problem)
	}
	if i.HelmBinary != "" && (!i.LocalHelm || strings.ContainsAny(i.HelmBinary, "\x00\r\n")) {
		return invalid("helmBinary requires localHelm and a valid executable path: " + name)
	}
	if len(i.HelmEnvironment) > 0 && (i.RepoServer != nil || !validHelmEnvironment(i.HelmEnvironment)) {
		return invalid(fmt.Sprintf("helmEnvironment cannot be combined with repoServer and allows at most %d unique, non-reserved names, each with exactly one of value or valueFromEnv (single-line, at most %d bytes): %s", MaxHelmEnvironment, MaxHelmEnvValueBytes, name))
	}
	if i.LocalHelm && i.RepoServer != nil {
		return invalid("localHelm and repoServer cannot be combined: " + name)
	}
	if i.RepoServer != nil {
		return i.RepoServer.validate(name)
	}
	return nil
}

// validate checks the repoServer of the instance entry name.
func (rpc *RepoServer) validate(name string) error {
	address := rpc.Address
	if forward := rpc.PortForward; forward != nil {
		parts := strings.Split(forward.Resource, "/")
		if rpc.Address != "" || rpc.Kubernetes == nil || rpc.CAFile != "" || len(parts) != 2 || (parts[0] != "service" && parts[0] != "deployment" && parts[0] != "pod") || !identifier.MatchString(parts[1]) || forward.Port < 1 || forward.Port > 65535 {
			return invalid("repoServer.portForward requires Kubernetes context, resource service|deployment|pod/name and port, without address or caFile: " + name)
		}
		address = "127.0.0.1:1" // Actual ephemeral port is assigned by kubectl.
	}
	host, port, err := net.SplitHostPort(address)
	number, portError := strconv.Atoi(port)
	problem := ""
	switch {
	case err != nil || portError != nil || number < 1 || number > 65535 || host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, "/@?# \t\r\n"):
		problem = "address must be host:port"
	case rpc.GRPCURL == "":
		problem = "grpcurl executable path is missing"
	case rpc.ContextFile == "" && rpc.Kubernetes == nil:
		problem = "contextFile or kubernetes is missing"
	}
	if problem != "" {
		return invalid("repoServer requires address host:port, grpcurl executable path and contextFile: " + name + ": repoServer." + problem)
	}
	if k := rpc.Kubernetes; k != nil {
		if rpc.ContextFile != "" || k.Kubectl == "" || k.Kubeconfig == "" || k.Context == "" || !identifier.MatchString(k.Namespace) {
			return invalid("repoServer.kubernetes requires kubectl, kubeconfig, explicit context and namespace, without contextFile: " + name)
		}
	}
	if rpc.Plaintext && (rpc.CAFile != "" || (host != "localhost" && host != "127.0.0.1" && host != "::1")) {
		return invalid("repoServer plaintext is allowed only on loopback without caFile: " + name)
	}
	return nil
}

func (c Config) validateDiscovery() error {
	if c.Discovery == nil {
		return nil
	}
	if c.Discovery.Mode != "repository" || len(c.Applications)+len(c.ApplicationSets) != 0 {
		return invalid("discovery.mode must be repository and cannot be combined with explicit applications or applicationSets")
	}
	projects := map[string]bool{}
	for i, project := range c.Discovery.Projects {
		if !identifier.MatchString(project) || projects[project] {
			return invalid(fmt.Sprintf("discovery.projects must contain unique project names: discovery.projects[%d]", i))
		}
		projects[project] = true
	}
	if s := c.Discovery.Selection; s != "" && s != "all" && s != SelectionChangedPaths {
		return invalid("discovery.selection must be all or changedPaths")
	}
	return nil
}

// validateSelections checks applications and applicationSets against the
// configured instance IDs.
func (c Config) validateSelections(instances map[string]bool) error {
	if (c.Discovery == nil && len(c.Applications)+len(c.ApplicationSets) == 0) || len(c.Applications)+len(c.ApplicationSets) > c.Render.MaxApplications {
		return invalid("provide explicit applications within maxApplications")
	}
	for _, list := range []struct {
		name, message string
		ids           []domain.ApplicationID
	}{
		{"applications", "applications must have unique IDs and reference a configured instance", c.Applications},
		{"applicationSets", "applicationSets must have unique IDs and reference a configured instance", c.ApplicationSets},
	} {
		seen := map[string]bool{}
		for i, a := range list.ids {
			problem := ""
			switch {
			case !instances[a.Instance]:
				problem = "instance is not a configured instance id"
			case !identifier.MatchString(a.Namespace):
				problem = "namespace must be an identifier"
			case !identifier.MatchString(a.Name):
				problem = "name must be an identifier"
			case seen[a.String()]:
				problem = "repeats an earlier entry"
			}
			if problem != "" {
				return invalid(fmt.Sprintf("%s: %s[%d] %s", list.message, list.name, i, problem))
			}
			seen[a.String()] = true
		}
	}
	return nil
}

// ValidToken accepts an absent credential or a bounded HTTP bearer token.
func ValidToken(token string) bool {
	if len(token) > 65536 {
		return false
	}
	for _, r := range token {
		if r <= 32 || r >= 127 {
			return false
		}
	}
	return true
}

func (r Render) Validate() error {
	if r.MaxConcurrency < 1 || r.MaxConcurrency > domain.MaxRenderConcurrency || r.MaxApplications < 1 || r.MaxApplications > domain.MaxRenderApplications || r.MaxResponseBytes < domain.MinRenderResponseBytes || r.MaxResponseBytes > domain.MaxRenderResponseBytes || r.MaxDocuments < 1 || r.MaxDocuments > domain.MaxRenderDocuments {
		return fmt.Errorf("invalid configuration: render limits must satisfy maxConcurrency 1–%d, maxApplications 1–%d, maxResponseBytes %d–%d, maxDocuments 1–%d", domain.MaxRenderConcurrency, domain.MaxRenderApplications, domain.MinRenderResponseBytes, domain.MaxRenderResponseBytes, domain.MaxRenderDocuments)
	}
	if r.MaxSourceSnapshotBytes != 0 && (r.MaxSourceSnapshotBytes < r.MaxResponseBytes || r.MaxSourceSnapshotBytes > MaxSourceSnapshotBytes) {
		return fmt.Errorf("invalid configuration: render.maxSourceSnapshotBytes must be between maxResponseBytes and %d", MaxSourceSnapshotBytes)
	}
	if r.MaxDepth < 1 || r.MaxDepth > domain.MaxRenderDepth {
		return fmt.Errorf("invalid configuration: render.maxDepth must be between 1 and %d", domain.MaxRenderDepth)
	}
	t, err := time.ParseDuration(r.Timeout)
	if err != nil || t < time.Second || t > 10*time.Minute {
		return fmt.Errorf("invalid configuration: render.timeout must be between 1s and 10m")
	}
	return nil
}
