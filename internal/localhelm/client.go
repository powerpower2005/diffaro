// Package localhelm renders Git Helm targets inside the Diffaro binary. It
// never uses Kubernetes credentials, repository scripts or repo-server RPC.
//
// Client reads immutable source archives through a gitsource.Reader, applies
// AppProject authorization and Argo CD render settings/capabilities read
// through Client.API, and renders with either the bundled engine (run in a
// Worker subprocess) or a trusted external Helm executable (External).
// Bundled renders of identical inputs, such as the base and head of an
// unchanged chart, start one worker (Client.memoized, cache.go), and
// External verifies identical inputs once (External.renderVerified); both
// share renderMemo (memo.go). Git contents use gitsource.Cache; fetchcache.Load
// shares fetches and their deterministic failures within the run.
// Value files and file parameters are resolved once per consumer by
// resolveInputs (inputs.go), so source planning, link checks, trimming and
// value collection agree on the files a render reads. collectValues
// (values.go) merges values and parameters like Argo CD, and trackDocuments
// (tracking.go) drops hook documents and adds Argo CD tracking.
package localhelm

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"

	"diffaro/internal/argocd"
	"diffaro/internal/argopolicy"
	"diffaro/internal/domain"
	"diffaro/internal/fetchcache"
	"diffaro/internal/github"
	"diffaro/internal/gitsource"
	"diffaro/internal/profile"
	"diffaro/internal/revision"
)

const HelmVersion = "v4.2.1"

type Client struct {
	// API is this instance's Argo CD Application API client. Client exposes
	// only the API methods forwarded below, so a new argocd method never
	// becomes a local Helm capability by accident.
	API           *argocd.Client
	Git           gitsource.Reader
	RepositoryURL string
	// RepositoryAliases are explicitly configured URLs of the selected
	// repository. Matching sources read archives from the configured API.
	RepositoryAliases []string
	// MirrorHosts are read-through mirrors of the GitHub host; sources on them
	// are read as the same owner/repository through the configured API.
	MirrorHosts []string
	Limit       int64
	// SnapshotLimit bounds retained source archives and dependency downloads
	// for this instance. Zero selects the 64 MiB default.
	SnapshotLimit int64
	External      *External
	// Execute is a test seam; production renders in a cancellable child process
	// of this same binary with an empty credential environment.
	Execute  func(context.Context, Request) ([]string, error)
	Download func(context.Context, string, int64) ([]byte, error)
	// Sources is the run-scoped GitHub content cache shared by instances. It
	// is used only when it was built for this Client's Git reader.
	Sources *gitsource.Cache
	// mu guards the maps below; it is never held during network I/O.
	mu       sync.Mutex
	private  *gitsource.Cache
	charged  map[string]bool
	resolved map[string]*fetchcache.Entry[string]
	// sparse counts files planned for file-by-file reads per repository
	// revision; past maxSparseBlobs the revision is read as an archive.
	sparse    map[string]int
	downloads map[string]*fetchcache.Entry[[]byte]
	// public is the dependency download client, shared by every download.
	public   *http.Client
	bytes    int64
	settings map[string]*fetchcache.Entry[argocd.RenderSettings]
	clusters map[string]*fetchcache.Entry[argocd.ClusterCapabilities]
	// indexes holds the parsed repository indexes among downloads.
	indexes map[string]*fetchcache.Entry[map[string]indexEntry]
	// renders holds bundled renders by input (memoized), counting their
	// retained manifests.
	renders renderMemo[[32]byte, memoRender]
}

// Application reads the installed Application through the Argo CD API
// (domain.Renderer).
func (c *Client) Application(ctx context.Context, id domain.ApplicationID) (domain.Application, error) {
	return c.API.Application(ctx, id)
}

// CheckProject verifies the AppProject through the Argo CD API before
// rendering (domain.ProjectChecker).
func (c *Client) CheckProject(ctx context.Context, app domain.Application) error {
	return c.API.CheckProject(ctx, app)
}

func (c *Client) BackendInfo() domain.BackendInfo {
	if c.External != nil {
		return domain.BackendInfo{Instance: c.API.Instance, Kind: "local-helm", ContextID: "external-helm/" + c.External.Version + "/sha256:" + c.External.SHA256, HelmVersion: c.External.Version, HelmSHA256: c.External.SHA256, HelmEnvironment: c.External.EnvironmentNames(), Verification: "local-context-unverified", Compatibility: "bounded-git-helm-external"}
	}
	return domain.BackendInfo{Instance: c.API.Instance, Kind: "local-helm", ContextID: "helm-" + HelmVersion + "/argocd-api-settings-and-capabilities", Verification: "local-context-unverified", Compatibility: "bounded-git-helm"}
}

func (c *Client) repository(raw string) (string, error) {
	base, _ := url.Parse(c.RepositoryURL)
	raw = profile.Unmirror(raw, c.RepositoryURL, c.MirrorHosts)
	if !profile.SameRepository(raw, c.RepositoryURL) && profile.SameRepositoryAny(raw, c.RepositoryURL, c.RepositoryAliases) {
		// An alias names the selected repository on another host. Never send
		// the GitHub credential there; read the same owner/repository via API.
		raw = c.RepositoryURL
	}
	if strings.HasPrefix(raw, "git@") && !strings.Contains(raw, "://") {
		host, p, ok := strings.Cut(strings.TrimPrefix(raw, "git@"), ":")
		if ok {
			raw = "ssh://git@" + host + "/" + p
		}
	}
	u, e := url.Parse(raw)
	if e != nil || u.Host != base.Host || (u.Scheme != "https" && u.Scheme != "ssh" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"))) || (u.User != nil && (u.Scheme != "ssh" || u.User.String() != "git")) || u.RawQuery != "" || u.Fragment != "" {
		return "", skip("source repository is outside the configured GitHub host")
	}
	name := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if len(strings.Split(name, "/")) != 2 || strings.Contains(name, "..") || strings.ContainsAny(name, "\\%\x00") {
		return "", skip("source repository has an unsupported path")
	}
	return name, nil
}

func (c *Client) ResolveSourceRevision(ctx context.Context, app domain.Application, repository, ref string) (string, error) {
	if app.ID.Instance != c.API.Instance {
		return "", domain.Fail(domain.CodeRoutingInvalid, "revision", "source resolver belongs to another instance", "correct instance routing", nil)
	}
	repo, err := c.repository(repository)
	if err != nil {
		return "", err
	}
	// Instance routing and repository mapping are checked above for every
	// Application; only the provider lookup is shared within this run.
	c.mu.Lock()
	if c.resolved == nil {
		c.resolved = map[string]*fetchcache.Entry[string]{}
	}
	c.mu.Unlock()
	return fetchcache.Load(ctx, &c.mu, c.resolved, repo+"\x00"+ref, func(ctx context.Context) (string, error) { return c.Git.Resolve(ctx, repo, ref) }, nil)
}

func (c *Client) Render(ctx context.Context, app domain.Application, sha string) ([]string, error) {
	return c.API.Bracket(ctx, app, drift(), drift(), func() ([]string, error) { return c.RenderDesired(ctx, app, sha) })
}

func (c *Client) RenderDesired(ctx context.Context, app domain.Application, sha string) ([]string, error) {
	plan, err := revision.PlanSources(ctx, app, domain.String(app.Source(), "repoURL"), sha, nil)
	if err != nil {
		return nil, err
	}
	return c.RenderDesiredSources(ctx, app, plan)
}

func (c *Client) RenderSources(ctx context.Context, app domain.Application, plan []domain.SourceRevision) ([]string, error) {
	return c.API.Bracket(ctx, app, drift(), drift(), func() ([]string, error) { return c.RenderDesiredSources(ctx, app, plan) })
}

func (c *Client) RenderDesiredSources(ctx context.Context, app domain.Application, plan []domain.SourceRevision) ([]string, error) {
	if err := c.checkPlan(app, plan); err != nil {
		return nil, err
	}
	endProject := domain.TraceRead(ctx, "project", "spec", app.Project())
	project, err := c.API.ProjectSpec(ctx, app.Project())
	endProject(func() string { return domain.DigestJSON(project) }, err)
	if err != nil {
		return nil, err
	}
	settings, capabilities, err := c.context(ctx, app)
	if err != nil {
		return nil, err
	}
	if err = authorizeSources(ctx, app, project, settings, capabilities, plan); err != nil {
		return nil, err
	}
	// Authorize every source before downloading any private files.
	request := Request{Application: app, Plan: plan, Settings: settings, Capabilities: capabilities, Repositories: map[string]map[string][]byte{}, Limit: c.Limit}
	if err = c.readSources(ctx, &request); err != nil {
		return nil, err
	}
	result, renderErr := c.execute(ctx, request)
	if renderErr != nil && !partial(renderErr) {
		return nil, renderErr
	}
	if err = c.API.ProjectUnchanged(ctx, app.Project(), project, drift()); err != nil {
		return nil, err
	}
	return result, renderErr
}

// checkPlan accepts a source plan for this instance that matches the
// Application's sources in everything but the selected revisions.
func (c *Client) checkPlan(app domain.Application, plan []domain.SourceRevision) error {
	if app.ID.Instance != c.API.Instance {
		return domain.Fail(domain.CodeRoutingInvalid, "render", "renderer belongs to another instance", "correct instance routing", nil)
	}
	if err := domain.SupportedSource(app); err != nil {
		return err
	}
	original, ok := app.Spec["sources"].([]any)
	if !ok || len(original) == 0 {
		original = []any{app.Source()}
	}
	if len(plan) != len(original) || len(plan) == 0 || len(plan) > 32 {
		return domain.Fail(domain.CodeSourceInvalid, "render", "local source plan does not match Application sources", "rebuild the immutable source plan", nil)
	}
	for i, s := range plan {
		if !s.Matches(original[i], i) {
			return domain.Fail(domain.CodeSourceInvalid, "render", "local source plan changed non-revision inputs", "rebuild the source plan", nil)
		}
	}
	return nil
}

// authorizeSources checks every source repository against the AppProject for
// the destination as Argo CD identifies it.
func authorizeSources(ctx context.Context, app domain.Application, project map[string]any, settings argocd.RenderSettings, capabilities argocd.ClusterCapabilities, plan []domain.SourceRevision) (err error) {
	authorized := domain.CloneSpec(app.Destination())
	authorized["name"], authorized["server"] = capabilities.Name, capabilities.Server
	end := domain.TraceStart(ctx, "project", "authorize-sources", app.Project())
	defer func() { end(err) }()
	for _, source := range plan {
		if err = argopolicy.Authorize(project, source.Repository, authorized, app.ID.Namespace, settings.ControllerNamespace); err != nil {
			return domain.SourceFailure(err, source)
		}
	}
	return nil
}

// readSources fills request.Repositories with the files each source
// repository revision needs, dependencies included, and rejects inputs read
// through repository links.
func (c *Client) readSources(ctx context.Context, request *Request) error {
	links := make([][]string, len(request.Plan))
	// revisionLinks holds the links of each repository revision read so far.
	// Its needs cover all of its sources, so a later source on the same
	// revision reuses the prepared files instead of reading them again.
	revisionLinks := map[string][]string{}
	needs := planNeeds(*request)
	for i, source := range request.Plan {
		repo, e := c.repository(source.Repository)
		if e != nil {
			return domain.SourceFailure(e, source)
		}
		key := source.Repository + "@" + source.Revision
		files, read := request.Repositories[key]
		if !read {
			endFiles := domain.TraceStart(ctx, "source", "files", repo+"@"+source.Revision)
			var sourceLinks []string
			files, sourceLinks, e = c.sourceFiles(ctx, repo, source.Revision, needs[key])
			if e != nil {
				e = domain.SourceFailure(e, source)
			}
			endFiles(e)
			if e != nil {
				return e
			}
			revisionLinks[key] = sourceLinks
		}
		links[i] = revisionLinks[key]
		if root := domain.String(source.Source, "path"); root != "" {
			if !domain.SafeRelativePath(root) {
				return renderError()
			}
			endDependencies := domain.TraceStart(ctx, "source", "dependencies", repo+"@"+source.Revision+":"+root)
			files, e = c.prepareDependencies(ctx, files, links[i], root, 0)
			if e != nil {
				e = domain.SourceFailure(e, source)
			}
			endDependencies(e)
			if e != nil {
				return e
			}
		}
		request.Repositories[key] = files
	}
	endLinks := domain.TraceStart(ctx, "source", "linked-inputs", "")
	e := linkedInputs(*request, links)
	endLinks(e)
	return e
}

// execute trims the request to the files Helm reads and renders it with the
// injected, bundled (worker subprocess) or external engine.
func (c *Client) execute(ctx context.Context, request Request) ([]string, error) {
	untrimmed := request.Repositories
	request.Repositories = trimRepositories(request.Plan, request.Repositories)
	// Which files a snapshot holds depends on the read strategy (file by
	// file or archive), which can depend on render order. The digest covers
	// the trimmed files Helm reads; "untrimmed" marks a snapshot whose read
	// set is not certain, so its digest can differ by strategy alone.
	coverage := "trimmed"
	if reflect.ValueOf(request.Repositories).Pointer() == reflect.ValueOf(untrimmed).Pointer() {
		coverage = "untrimmed"
	}
	domain.TraceRead(ctx, "source", "render-inputs", coverage)(func() string { return repositoriesDigest(request.Repositories) }, nil)
	execute, engine := c.Execute, "injected"
	if execute == nil {
		execute, engine = bundledProcess, "bundled"
		if c.External != nil {
			// The external engine keeps its own cache of renders that passed
			// its determinism check (renderVerified).
			execute, engine = c.External.Render, "external"
		} else if coverage == "trimmed" {
			execute = c.memoized
		}
	}
	endHelm := domain.TraceRead(ctx, "render", "helm", engine)
	result, err := execute(ctx, request)
	endHelm(func() string { return domain.DigestJSON(result) }, err)
	return result, err
}

func (c *Client) context(ctx context.Context, app domain.Application) (argocd.RenderSettings, argocd.ClusterCapabilities, error) {
	c.mu.Lock()
	if c.settings == nil {
		c.settings = map[string]*fetchcache.Entry[argocd.RenderSettings]{}
		c.clusters = map[string]*fetchcache.Entry[argocd.ClusterCapabilities]{}
	}
	c.mu.Unlock()
	endSettings := domain.TraceRead(ctx, "context", "settings", c.API.Instance)
	settings, err := fetchcache.Load(ctx, &c.mu, c.settings, "", func(ctx context.Context) (argocd.RenderSettings, error) {
		s, err := c.API.RenderSettings(ctx)
		if err == nil && (s.ControllerNamespace == "" || s.AppLabelKey == "" || (s.TrackingMethod != "annotation" && s.TrackingMethod != "label" && s.TrackingMethod != "annotation+label")) {
			err = skip("Argo CD API did not expose a complete tracking context")
		}
		return s, err
	}, nil)
	endSettings(func() string { return domain.DigestJSON(settings) }, err)
	if err != nil {
		return settings, argocd.ClusterCapabilities{}, err
	}
	dest := app.Destination()
	server, name := domain.String(dest, "server"), domain.String(dest, "name")
	key := server + "|" + name
	c.mu.Lock()
	_, known := c.clusters[key]
	full := !known && len(c.clusters) >= 1000
	c.mu.Unlock()
	if full {
		return settings, argocd.ClusterCapabilities{}, skip("local capability snapshot limit exceeded")
	}
	endCapabilities := domain.TraceRead(ctx, "context", "capabilities", key)
	caps, err := fetchcache.Load(ctx, &c.mu, c.clusters, key, func(ctx context.Context) (argocd.ClusterCapabilities, error) {
		return c.API.ClusterCapabilities(ctx, server, name)
	}, nil)
	if err != nil {
		var failure *domain.Failure
		if errors.As(err, &failure) && failure.Issue.Code == domain.CodeClusterCapabilitiesUnavailable {
			err = skip("destination API capabilities are missing, incomplete or unhealthy")
		}
	}
	endCapabilities(func() string { return domain.DigestJSON(caps) }, err)
	if err != nil {
		return settings, caps, err
	}
	return settings, caps, nil
}

// sources returns the shared run cache when it reads through this Client's
// own Git reader, and a private per-instance cache otherwise.
func (c *Client) sources() *gitsource.Cache {
	if c.Sources.ReadsWith(c.Git) {
		return c.Sources
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.private == nil {
		c.private = gitsource.New(c.Git, c.snapshotLimit())
	}
	return c.private
}

func (c *Client) archive(ctx context.Context, repository, sha string) (map[string][]byte, []string, error) {
	files, links, err := c.sources().Archive(ctx, repository, sha)
	if err != nil {
		return nil, nil, err
	}
	if err := c.charge("archive:"+repository+"@"+sha, gitsource.ArchiveSize(files)); err != nil {
		return nil, nil, err
	}
	return files, links, nil
}

// charge counts the bytes of a key once against this instance's source
// snapshot limit, whether or not another instance already read them.
func (c *Client) charge(key string, size int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.charged[key] {
		return nil
	}
	if limit := c.snapshotLimit(); c.bytes+size > limit {
		return domain.LimitError(domain.CodeLimitExceeded, "source", "instance source snapshot", "render.maxSourceSnapshotBytes", c.bytes+size, limit, "bytes", false, domain.MaxRenderSourceSnapshotBytes)
	}
	if c.charged == nil {
		c.charged = map[string]bool{}
	}
	c.charged[key] = true
	c.bytes += size
	return nil
}

func (c *Client) snapshotLimit() int64 {
	if c.SnapshotLimit > 0 {
		return c.SnapshotLimit
	}
	return profile.DefaultSourceSnapshotBytes
}

func skip(reason string) error {
	return skipWith(reason, nil)
}

// nondeterministicSkip is skip for a template that called a randomness, time
// or live-dependent function; errors.Is(err, errNondeterministic) finds it.
func nondeterministicSkip(reason string) error {
	return skipWith(reason, errNondeterministic)
}

func skipWith(reason string, cause error) error {
	reason = strings.TrimPrefix(reason, "Local Helm analysis skipped: ")
	return &domain.RenderSkipped{Reason: "Local Helm analysis skipped: " + reason, Cause: domain.Fail(domain.CodeLocalContextUnavailable, "render", reason, "provide readable source files and supported API render context; do not substitute another cluster or installed spec", cause)}
}
func drift() error {
	return domain.Fail(domain.CodeRenderContextChanged, "render", "Application or AppProject changed during local rendering", "retry after configuration is stable", nil)
}

// The capabilities preview discovers on a local Helm backend.
var (
	_ domain.Renderer               = (*Client)(nil)
	_ domain.ProjectChecker         = (*Client)(nil)
	_ domain.DesiredRenderer        = (*Client)(nil)
	_ domain.MultiSourceRenderer    = (*Client)(nil)
	_ domain.SourceRevisionResolver = (*Client)(nil)
	_ domain.BackendInfoProvider    = (*Client)(nil)
)

var _ gitsource.Reader = (*github.Client)(nil)
var _ gitsource.TreeReader = (*github.Client)(nil)
