package profile

import (
	"slices"

	"diffaro/internal/domain"
)

// Shape describes c by counts and setting names only, for the report
// generator. Credentials, hosts, repositories, Application names
// and values never appear.
func (c Config) Shape() domain.ProfileShape {
	shape := domain.ProfileShape{Instances: len(c.Instances), Backends: []string{}}
	feature := func(name string, used bool) {
		if used {
			shape.Features = append(shape.Features, name)
		}
	}
	var repoServer, helmEnvironment bool
	for _, instance := range c.Instances {
		kind := "argocd-application-api"
		switch {
		case instance.RepoServer != nil:
			kind, repoServer = domain.BackendRepoServer, true
		case instance.LocalHelm && instance.HelmBinary != "":
			kind = domain.BackendLocalHelm + "-external"
		case instance.LocalHelm:
			kind = domain.BackendLocalHelm
		}
		if !slices.Contains(shape.Backends, kind) {
			shape.Backends = append(shape.Backends, kind)
		}
		helmEnvironment = helmEnvironment || len(instance.HelmEnvironment) > 0
	}
	slices.Sort(shape.Backends)
	if c.Discovery != nil {
		shape.Selection = c.Discovery.Selection
		if shape.Selection == "" {
			shape.Selection = "all"
		}
	}
	feature("applicationSets", len(c.ApplicationSets) > 0)
	feature("excludePreexistingFromExit", c.Failures.ExcludePreexistingFromExit)
	feature("helmEnvironment", helmEnvironment)
	feature("localRepositories", len(c.Git.LocalRepositories) > 0)
	feature("mirrorHosts", len(c.Git.MirrorHosts) > 0)
	feature("policies", len(c.Policies) > 0)
	feature("repoServer", repoServer)
	feature("repositoryAliases", len(c.Git.RepositoryAliases) > 0)
	feature("sensitivePaths", len(c.SensitivePaths) > 0)
	defaults := DefaultRender()
	limit := func(name string, changed bool) {
		if changed {
			shape.ChangedLimits = append(shape.ChangedLimits, name)
		}
	}
	limit("render.maxApplications", c.Render.MaxApplications != defaults.MaxApplications)
	limit("render.maxConcurrency", c.Render.MaxConcurrency != defaults.MaxConcurrency)
	limit("render.maxDepth", c.Render.MaxDepth != defaults.MaxDepth)
	limit("render.maxDocuments", c.Render.MaxDocuments != defaults.MaxDocuments)
	limit("render.maxResponseBytes", c.Render.MaxResponseBytes != defaults.MaxResponseBytes)
	limit("render.maxSourceSnapshotBytes", c.Render.MaxSourceSnapshotBytes != defaults.MaxSourceSnapshotBytes)
	limit("render.timeout", c.Render.Timeout != defaults.Timeout)
	return shape
}
