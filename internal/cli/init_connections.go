package cli

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/onboarding"
	"diffaro/internal/profile"
)

// setDiscoveryOptions saves --repository-alias and --selection in a
// generated repository-discovery profile and revalidates it.
func setDiscoveryOptions(c *profile.Config, o *initOptions) error {
	if len(o.aliases) == 0 && o.selection == "" {
		return nil
	}
	if len(o.aliases) > 0 {
		c.Git.RepositoryAliases = append([]string{}, o.aliases...)
	}
	if o.selection != "" {
		if c.Discovery == nil {
			return fmt.Errorf("--selection requires a repository-discovery profile created with --pr")
		}
		discovery := *c.Discovery
		discovery.Selection = o.selection
		c.Discovery = &discovery
	}
	return c.Validate()
}

// setMirrorHosts saves --mirror-host as git.mirrorHosts. A host that is
// already the repositoryURL host, as when --application selects an
// Application whose source reads through that mirror, needs no entry; it is
// returned instead of making the profile invalid.
func setMirrorHosts(c *profile.Config, o *initOptions) (skipped []string) {
	var hosts []string
	repository, _ := url.Parse(c.Git.RepositoryURL)
	for _, host := range o.mirrorHosts {
		if mirror, err := url.Parse(host); err == nil && repository != nil && strings.EqualFold(mirror.Host, repository.Host) {
			skipped = append(skipped, host)
			continue
		}
		hosts = append(hosts, host)
	}
	if len(hosts) > 0 {
		c.Git.MirrorHosts = hosts
	}
	return skipped
}

// initFromConnections writes a repository-discovery profile from a trusted
// connection catalog without contacting any instance.
func initFromConnections(f *flag.FlagSet, o *initOptions, stdout, stderr io.Writer) int {
	// The catalog owns all per-instance options; combining both connection
	// paths would make it unclear which credentials or backend was chosen.
	conflict := false
	f.Visit(func(option *flag.Flag) {
		switch option.Name {
		case "git-token", "argocd-token", "instance-token", "connections", "local-children", "child-namespace", "config", "pr", "git-api", "git-token-env", "max-applications", "max-response-bytes", "max-source-snapshot-bytes", "timeout", "max-concurrency", "repository-alias", "mirror-host", "selection", "local-helm", "helm-binary":
		default:
			conflict = true
		}
	})
	if conflict || o.prURL == "" || (!o.localChildren && o.namespaceProvided) {
		fmt.Fprintln(stderr, "init --connections requires --pr; configure per-instance options in the catalog; --child-namespace requires --local-children")
		return domain.ExitInvalidConfiguration
	}
	catalog, err := profile.LoadConnections(o.connections)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return domain.ExitInvalidConfiguration
	}
	if o.localHelm && !selectLocalHelm(catalog.Instances, o.helmBinary) {
		fmt.Fprintln(stderr, "--local-helm cannot be combined with repoServer")
		return domain.ExitInvalidConfiguration
	}
	c, err := onboarding.ConnectionsProfile(catalog, o.prURL, o.gitAPI, o.gitToken, o.localChildren, o.childNamespace, o.limits)
	if err == nil {
		err = setDiscoveryOptions(&c, o)
	}
	if err == nil && len(o.mirrorHosts) > 0 {
		setMirrorHosts(&c, o)
		err = c.Validate()
	}
	if err == nil {
		err = checkNewProfilePath(o.config)
	}
	var data []byte
	if err == nil {
		data, c, err = o.credentials.persist(f, c)
	}
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return domain.ExitInvalidConfiguration
	}
	code := writeInitializedProfile(o.config, data, c, false, false, o.prURL, stdout, stderr)
	if code == domain.ExitSuccess {
		fmt.Fprintln(stdout, "Created offline; connections were not checked. Run doctor --render to verify each instance and review analysis coverage.")
	}
	return code
}
