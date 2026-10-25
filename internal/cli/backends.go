package cli

import (
	"context"
	"os/exec"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
	"diffaro/internal/github"
	"diffaro/internal/gitlocal"
	"diffaro/internal/gitsource"
	"diffaro/internal/httpx"
	"diffaro/internal/localhelm"
	"diffaro/internal/preview"
	"diffaro/internal/profile"
	"diffaro/internal/reposerver"
)

// sourceReader reads repository content: the Git provider API, or local
// repositories in front of it (gitlocal).
type sourceReader interface {
	gitsource.Reader
	Tree(ctx context.Context, repository, revision string) ([]github.TreeEntry, error)
	Blob(ctx context.Context, repository, sha string) ([]byte, error)
	File(ctx context.Context, repository, revision, file string) ([]byte, bool, error)
}

// backends holds the adapters built for one command run. Each Argo CD instance
// gets its own client, credentials and render backend.
type backends struct {
	git *github.Client
	// source reads repository content for local Helm, changed-path
	// selection and input changes; it is git unless git.localRepositories
	// is configured.
	source         sourceReader
	local          *gitlocal.Reader
	notes          []string
	renderers      map[string]domain.Renderer
	inventories    map[string]domain.ApplicationInventory
	versionReaders map[string]domain.BackendVersionReader
	// setupIssues records instances whose configured backend failed to start;
	// their renderer is an unavailableBackend so other instances still run.
	setupIssues []domain.Issue
	closers     []func()
	// sources is the run-scoped GitHub content cache read through git and
	// shared by every local Helm instance.
	sources *gitsource.Cache
}

// buildBackends selects the render backend per instance: the Argo CD
// Application API by default, local Helm when localHelm is set, or repo-server
// when repoServer is set. profile validation rejects setting both.
func buildBackends(ctx context.Context, c profile.Config, helmEnvironment map[string][]string) *backends {
	b := &backends{
		renderers:      map[string]domain.Renderer{},
		inventories:    map[string]domain.ApplicationInventory{},
		versionReaders: map[string]domain.BackendVersionReader{},
	}
	b.git = &github.Client{HTTP: httpx.NewWithTokenEnv(c.Git.APIURL, c.Git.Token, c.Git.TokenEnv, c.Timeout(), c.Render.MaxResponseBytes)}
	b.source = b.git
	if len(c.Git.LocalRepositories) > 0 {
		executable := c.Git.GitExecutable
		if executable == "" {
			executable, _ = exec.LookPath("git")
		}
		if executable == "" {
			b.notes = append(b.notes, "git.localRepositories are not used: no git executable was found; set git.gitExecutable. Sources were read through the Git provider API.")
		} else {
			paths := make(map[string]string, len(c.Git.LocalRepositories))
			for _, local := range c.Git.LocalRepositories {
				paths[local.Name] = local.Path
			}
			b.local = &gitlocal.Reader{API: b.git, Executable: executable, Paths: paths, Limit: c.Render.MaxResponseBytes}
			b.source = b.local
		}
	}
	// One token and host read GitHub for every instance, so content keyed by
	// immutable repository revisions and blob SHAs is shared. It retains no
	// more than the instances' snapshot limits together; each instance still
	// authorizes and charges its own reads.
	var retained int64
	for _, instance := range c.Instances {
		if instance.LocalHelm {
			retained += c.Render.SourceSnapshotBytes()
		}
	}
	b.sources = gitsource.New(b.source, max(retained, 1))
	for _, instance := range c.Instances {
		api := &argocd.Client{Instance: instance.ID, HTTP: httpx.NewWithTokenEnv(instance.Endpoint, instance.Token, instance.TokenEnv, c.Timeout(), c.Render.MaxResponseBytes)}
		b.inventories[instance.ID] = api
		b.versionReaders[instance.ID] = api
		b.renderers[instance.ID] = api
		if instance.LocalHelm {
			b.addLocalHelm(ctx, c, instance, api, helmEnvironment[instance.ID])
		}
		if instance.RepoServer != nil {
			b.addRepoServer(c, instance, api)
		}
	}
	return b
}

func (b *backends) addLocalHelm(ctx context.Context, c profile.Config, instance profile.Instance, api *argocd.Client, environment []string) {
	backend := &localhelm.Client{API: api, Git: b.source, RepositoryURL: c.Git.RepositoryURL, RepositoryAliases: c.Git.Aliases(), MirrorHosts: c.Git.MirrorHosts, Limit: c.Render.MaxResponseBytes, SnapshotLimit: c.Render.SourceSnapshotBytes(), Sources: b.sources}
	if instance.HelmBinary != "" {
		external, err := localhelm.NewExternal(ctx, instance.HelmBinary)
		if err != nil {
			b.unavailable(c, instance.ID, domain.BackendLocalHelm, err)
			return
		}
		b.closers = append(b.closers, external.Close)
		external.Environment = environment
		backend.External = external
	}
	b.renderers[instance.ID] = backend
}

func (b *backends) addRepoServer(c profile.Config, instance profile.Instance, api *argocd.Client) {
	backend, err := reposerver.New(api, *instance.RepoServer, c.Render.MaxResponseBytes)
	if err != nil {
		b.unavailable(c, instance.ID, domain.BackendRepoServer, err)
		return
	}
	b.renderers[instance.ID] = backend
	b.closers = append(b.closers, func() { _ = backend.Close() })
}

func (b *backends) unavailable(c profile.Config, instance, kind string, cause error) {
	b.renderers[instance] = unavailableBackend{instance: instance, kind: kind, cause: cause}
	b.setupIssues = append(b.setupIssues, preview.ContextualIssue(cause, domain.ApplicationID{Instance: instance}, c.Git.RepositoryURL, ""))
}

// close releases external processes in reverse creation order.
func (b *backends) close() {
	for i := len(b.closers) - 1; i >= 0; i-- {
		b.closers[i]()
	}
}
