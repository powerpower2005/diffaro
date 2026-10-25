// Package reposerver renders desired specs through Argo CD's repo-server RPC.
// All repository credentials and cluster capabilities come from a trusted,
// instance-local snapshot, never from the pull request. The snapshot is either
// a context file or, with repoServer.kubernetes, collected from an explicit
// kubeconfig context; repoServer.portForward reaches the repo-server through
// kubectl port-forward on that context. selectRepository (credentials.go)
// picks the repository Secret and credential template for a source.
// Stored multi-source Applications render through the Argo CD API client
// (Client.API); Client forwards only the API methods preview uses.
package reposerver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"

	"diffaro/internal/argocd"
	"diffaro/internal/argopolicy"
	"diffaro/internal/determinism"
	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

type Snapshot struct {
	Version         string    `json:"version"`
	Instance        string    `json:"instance"`
	ContextID       string    `json:"contextID"`
	ArgoCDNamespace string    `json:"argocdNamespace"`
	Bindings        []Binding `json:"bindings"`
}
type Binding struct {
	resolvedName         string
	resolvedServer       string // Authenticated name resolution; never provided by a PR or context file.
	Project              string `json:"project"`
	ApplicationNamespace string `json:"applicationNamespace"`
	// Nil permits any Application name within the other exact binding selectors.
	// A non-empty list optionally narrows that scope; explicit [] is invalid.
	Applications  []string       `json:"applications"`
	RepositoryURL string         `json:"repositoryURL"`
	Destination   map[string]any `json:"destination"`
	Request       map[string]any `json:"request"`
}
type Response struct {
	Manifests  []string `json:"manifests"`
	Revision   string   `json:"revision"`
	SourceType string   `json:"sourceType"`
}
type Client struct {
	maxResponseBytes int64
	forward          *portForward
	// API is this instance's Argo CD Application API client. Client exposes
	// only the API methods forwarded below, so a new argocd method never
	// becomes a repo-server capability by accident.
	API        *argocd.Client
	kubernetes *kubernetesContext
	snapshot   Snapshot
	// schema is the wire schema file shared by this Client's RPCs.
	schema  *protoSchema
	invoke  func(context.Context, map[string]any) (Response, error)
	resolve func(context.Context, map[string]any) (Response, error)
}

func (c *Client) Close() error {
	if c.forward != nil {
		c.forward.close()
	}
	c.schema.remove()
	return nil
}

func New(base *argocd.Client, config profile.RepoServer, limit int64) (*Client, error) {
	client := &Client{API: base, maxResponseBytes: limit, schema: &protoSchema{}}
	if config.Kubernetes != nil {
		if config.ContextFile != "" {
			return nil, invalid("choose either Kubernetes collection or contextFile")
		}
		collector := newKubernetesContext(*config.Kubernetes)
		collector.capabilities = base.ClusterCapabilities
		collector.tracking = base.EffectiveTrackingMethod
		client.kubernetes = collector
		client.snapshot = Snapshot{Version: "v1alpha1", Instance: base.Instance, ContextID: "kubernetes-read-snapshot", ArgoCDNamespace: config.Kubernetes.Namespace}
		if config.PortForward != nil {
			client.forward = &portForward{config: config}
		}
	} else {
		snapshot, err := loadSnapshot(config, base.Instance)
		if err != nil {
			return nil, err
		}
		client.snapshot = snapshot
	}
	client.invoke, client.resolve = client.rpc(config, limit, Method), client.rpc(config, limit, resolveMethod)
	return client, nil
}

// rpc invokes method on this Client's repo-server connection.
func (c *Client) rpc(config profile.RepoServer, limit int64, method string) func(context.Context, map[string]any) (Response, error) {
	return func(ctx context.Context, request map[string]any) (Response, error) {
		connection, err := c.connection(ctx, config)
		if err != nil {
			return Response{}, err
		}
		return processMethodInvoker(connection, limit, method, c.schema)(ctx, request)
	}
}

// connection is config with the address of the port-forward tunnel, when
// the repo-server is reached through one.
func (c *Client) connection(ctx context.Context, config profile.RepoServer) (profile.RepoServer, error) {
	if c.forward == nil {
		return config, nil
	}
	address, err := c.forward.address(ctx)
	if err != nil {
		return config, err
	}
	config.Address = address
	return config, nil
}

// loadSnapshot reads and validates a trusted repo-server context file and
// the grpcurl and CA files it is used with.
func loadSnapshot(config profile.RepoServer, instance string) (Snapshot, error) {
	var snapshot Snapshot
	f, err := os.Open(config.ContextFile)
	if err != nil {
		return snapshot, invalid("cannot open trusted repo-server context file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if err != nil || len(data) > 16<<20 {
		return snapshot, invalid("repo-server context exceeds the 16 MiB limit or cannot be read")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&snapshot); err != nil {
		return snapshot, invalid("repo-server context must be valid JSON with known fields")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return snapshot, invalid("repo-server context must contain one JSON object")
	}
	if err = snapshot.Validate(instance); err != nil {
		return snapshot, err
	}
	if info, e := os.Stat(config.GRPCURL); e != nil || info.IsDir() {
		return snapshot, invalid("grpcurl executable path is unavailable")
	}
	if config.CAFile != "" {
		if info, e := os.Stat(config.CAFile); e != nil || info.IsDir() {
			return snapshot, invalid("repo-server CA file is unavailable")
		}
	}
	return snapshot, nil
}

var safeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,252}$`)

func (s Snapshot) Validate(instance string) error {
	if s.Version != "v1alpha1" || s.Instance != instance || !safeName.MatchString(s.ContextID) || !safeName.MatchString(s.ArgoCDNamespace) || len(s.Bindings) == 0 {
		return invalid("repo-server context version, instance, contextID, namespace or bindings are invalid")
	}
	for _, b := range s.Bindings {
		if !safeName.MatchString(b.Project) || !safeName.MatchString(b.ApplicationNamespace) || !profile.ValidEndpoint(b.RepositoryURL) || len(b.Destination) == 0 {
			return invalid("repo-server binding requires exact project, Application namespace, repository and destination selectors")
		}
		if b.Applications != nil && len(b.Applications) == 0 {
			return invalid("omit applications for scope-based matching or provide a non-empty exact name list")
		}
		for _, name := range b.Applications {
			if !safeName.MatchString(name) {
				return invalid("repo-server Application allowlist must contain exact names")
			}
		}
		if domain.String(b.Destination, "namespace") == "" || (domain.String(b.Destination, "name") == "" && domain.String(b.Destination, "server") == "") {
			return invalid("repo-server binding requires destination cluster and namespace")
		}
		if domain.String(b.Destination, "name") != "" && domain.String(b.Destination, "server") != "" {
			return invalid("repo-server binding must select destination by either name or server")
		}
		for k := range b.Destination {
			if k != "name" && k != "server" && k != "namespace" {
				return invalid("unsupported destination field in repo-server context")
			}
		}
		repo, _ := b.Request["repo"].(map[string]any)
		if domain.String(repo, "repo") != b.RepositoryURL {
			return invalid("repo-server credential repository must equal binding repository")
		}
		for key := range b.Request {
			switch key {
			case "repo", "repos", "helmRepoCreds", "appLabelKey", "trackingMethod", "kubeVersion", "apiVersions", "installationID", "helmOptions", "enabledSourceTypes":
			default:
				return invalid("repo-server context contains a caller-owned or unsupported request field")
			}
		}
		for _, key := range []string{"appLabelKey", "trackingMethod", "kubeVersion", "installationID"} {
			if _, ok := b.Request[key].(string); !ok {
				return invalid("repo-server context must explicitly capture tracking, installation and Kubernetes version settings")
			}
		}
		if domain.String(b.Request, "kubeVersion") == "" {
			return invalid("repo-server Kubernetes version must not be empty")
		}
		for _, key := range []string{"apiVersions", "repos", "helmRepoCreds"} {
			if _, ok := b.Request[key].([]any); !ok {
				return invalid("repo-server context must explicitly capture API versions and Helm dependency credentials")
			}
		}
		if versions := b.Request["apiVersions"].([]any); len(versions) == 0 {
			return invalid("repo-server API capabilities must not be empty")
		} else {
			for _, v := range versions {
				if str, ok := v.(string); !ok || str == "" {
					return invalid("repo-server API capabilities must be strings")
				}
			}
		}
		if _, ok := b.Request["helmOptions"].(map[string]any); !ok {
			return invalid("repo-server context must explicitly capture Helm options")
		}
		types, ok := b.Request["enabledSourceTypes"].(map[string]any)
		if !ok || types["Helm"] != true {
			return invalid("repo-server context must explicitly enable Helm")
		}
		for name, enabled := range types {
			if name != "Helm" && enabled != false {
				return invalid("only Helm may be enabled in this repo-server backend")
			}
		}
	}
	return nil
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

// RenderSources renders a stored multi-source Application through the Argo CD
// API's sourcePositions/revisions manifests endpoint, not the repo-server RPC.
// Only new or reconfigured desired specs use the RPC (RenderDesiredSources).
func (c *Client) RenderSources(ctx context.Context, app domain.Application, plan []domain.SourceRevision) ([]string, error) {
	return c.API.RenderSources(ctx, app, plan)
}

func (c *Client) BackendInfo() domain.BackendInfo {
	return domain.BackendInfo{Instance: c.API.Instance, Kind: "argocd-repo-server", ContextID: c.snapshot.ContextID, Verification: "live-unverified"}
}

func (c *Client) Render(ctx context.Context, app domain.Application, revision string) ([]string, error) {
	return c.API.Bracket(ctx, app, drift("Application spec changed before repo-server render"), drift("Application spec changed during repo-server render"), func() ([]string, error) {
		return c.RenderDesired(ctx, app, revision)
	})
}

func (c *Client) RenderDesired(ctx context.Context, app domain.Application, revision string) ([]string, error) {
	if !domain.IsCommitSHA(revision) {
		return nil, domain.Fail(domain.CodeRevisionInvalid, "revision", "repo-server requires an immutable commit SHA", "resolve the revision before rendering", nil)
	}
	if app.ID.Instance != c.API.Instance {
		return nil, otherInstance()
	}
	if !safeName.MatchString(app.ID.Name) {
		return nil, invalid("desired Application name is invalid")
	}
	if sources, ok := app.Spec["sources"].([]any); ok && len(sources) > 0 {
		return nil, domain.Fail(domain.CodeUnsupportedSource, "selection", "repo-server backend requires explicit Git Helm rendering sources", "use a supported source without CMP, hydration or ignore rules", nil)
	}
	if err := domain.SupportedSource(app); err != nil {
		return nil, err
	}
	selected, project, err := c.authorizedBinding(ctx, app)
	if err != nil {
		return nil, err
	}
	manifests, err := c.renderBoundSource(ctx, app, revision, selected, project, nil, false, false)
	if err != nil {
		return nil, err
	}
	if err = c.API.ProjectUnchanged(ctx, app.Project(), project, drift("AppProject changed during repo-server render")); err != nil {
		return nil, err
	}
	return manifests, nil
}

func (c *Client) renderBoundSource(ctx context.Context, app domain.Application, revision string, selected *Binding, project, refs map[string]any, multi, valuesOnly bool) ([]string, error) {
	dest := app.Destination()
	request := domain.CloneSpec(selected.Request)
	if c.kubernetes != nil {
		repos, creds, err := c.kubernetes.helmDependencies(project, app.Project())
		if err != nil {
			return nil, err
		}
		request["repos"], request["helmRepoCreds"] = repos, creds
	}
	source := domain.CloneSpec(app.Source())
	source["targetRevision"] = revision
	// REST/YAML uses an ordinary JSON object; protobuf RawExtension uses bytes.
	// This is wire encoding only: Helm precedence and merging remain in Argo CD.
	helm, _ := source["helm"].(map[string]any)
	if values, exists := helm["valuesObject"]; exists && values != nil {
		if _, ok := values.(map[string]any); !ok {
			return nil, domain.Fail(domain.CodeUnsupportedSource, "render", "Helm valuesObject must be an object", "provide a valid Helm valuesObject", nil)
		}
		raw, err := json.Marshal(values)
		if err != nil {
			return nil, invalid("cannot encode Helm valuesObject")
		}
		helm["valuesObject"] = map[string]any{"raw": base64.StdEncoding.EncodeToString(raw)}
	}
	request["applicationSource"], request["revision"] = source, revision
	name := app.ID.Name
	if app.ID.Namespace != c.snapshot.ArgoCDNamespace {
		name = app.ID.Namespace + "_" + name
	}
	request["appName"], request["namespace"] = name, domain.String(dest, "namespace")
	request["projectName"], request["projectSourceRepos"] = app.Project(), project["sourceRepos"]
	request["hasMultipleSources"], request["noCache"], request["noRevisionCache"] = multi, true, true
	if multi {
		request["refSources"] = domain.CloneSpec(refs)
	}
	if valuesOnly {
		return c.generate(ctx, request, domain.String(source, "repoURL"), revision, valuesOnly)
	}
	// A template that calls a randomness function, such as a generated
	// password, renders differently each time. repo-server gives no template
	// source to screen, so every render is repeated and the fields that
	// differ are not compared.
	manifests, again, err := c.generateTwice(ctx, request, domain.String(source, "repoURL"), revision)
	if err != nil || len(manifests) == 0 {
		return manifests, err
	}
	first, second := strings.Join(manifests, "\n---\n"), strings.Join(again, "\n---\n")
	marked, same, ok := determinism.Compare(first, second)
	if same {
		return manifests, nil
	}
	if !ok {
		reason := "repo-server rendered different resources for identical inputs, as a template with a random resource name does"
		return nil, &domain.RenderSkipped{Reason: "repo-server analysis skipped: " + reason, Cause: domain.Fail(domain.CodeRenderNondeterministic, "render", reason, "use a chart whose resource names do not depend on randomness, or review this Application manually", nil)}
	}
	documents := []string{}
	for _, document := range strings.Split(marked, "---\n") {
		if document = strings.TrimSpace(document); document != "" {
			documents = append(documents, document)
		}
	}
	return documents, nil
}

// generateTwice issues the two GenerateManifest calls of a Helm render at the
// same time, so the render timeout covers about one render rather than two.
// The first failure cancels the other call and is returned.
func (c *Client) generateTwice(ctx context.Context, request map[string]any, repoURL, revision string) (first, second []string, err error) {
	both, cancel := context.WithCancel(ctx)
	defer cancel()
	var once sync.Once
	var wg sync.WaitGroup
	results := make([][]string, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var callErr error
			defer func() {
				if callErr != nil {
					once.Do(func() { err = callErr; cancel() })
				}
			}()
			// A panic fails this render, not the run.
			defer domain.RecoverAs(&callErr, "render")
			results[i], callErr = c.generate(both, request, repoURL, revision, false)
		}()
	}
	wg.Wait()
	if err != nil {
		return nil, nil, err
	}
	return results[0], results[1], nil
}

// generate is one GenerateManifest call for request, checked against the
// requested revision and source type.
func (c *Client) generate(ctx context.Context, request map[string]any, repoURL, revision string, valuesOnly bool) ([]string, error) {
	end := domain.TraceRead(ctx, "render", "rpc", repoURL+"@"+revision)
	// A copy per call keeps a repeated render's inputs identical.
	response, err := c.invoke(ctx, domain.CloneSpec(request))
	end(func() string { return domain.DigestJSON(response.Manifests) }, err)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(response.Revision, revision) || (!valuesOnly && response.SourceType != "Helm") || (valuesOnly && (len(response.Manifests) != 0 || response.SourceType != "")) {
		return nil, domain.Fail(domain.CodeRenderResponseMismatch, "render", "repo-server returned a different revision or source type", "check repo-server RPC compatibility and resolved revision", nil)
	}
	if response.Manifests == nil {
		response.Manifests = []string{}
	}
	return response.Manifests, nil
}

// authorizedBinding selects app's binding and authorizes it against a fresh
// read of app's AppProject, which it returns for the drift check after the
// RPC.
func (c *Client) authorizedBinding(ctx context.Context, app domain.Application) (*Binding, map[string]any, error) {
	selected, err := c.selectBinding(ctx, app)
	if err != nil {
		return nil, nil, err
	}
	project, err := c.projectSpec(ctx, app.Project())
	if err != nil {
		return nil, nil, err
	}
	if err = c.authorize(ctx, selected, project); err != nil {
		return nil, nil, err
	}
	return selected, project, nil
}

// projectSpec reads the AppProject name as a traced project step.
func (c *Client) projectSpec(ctx context.Context, name string) (map[string]any, error) {
	end := domain.TraceRead(ctx, "project", "spec", name)
	project, err := c.API.ProjectSpec(ctx, name)
	end(func() string { return domain.DigestJSON(project) }, err)
	return project, err
}

// selectBinding returns the one trusted binding of app's project, namespace,
// repository, destination and name.
func (c *Client) selectBinding(ctx context.Context, app domain.Application) (_ *Binding, err error) {
	end := domain.TraceStart(ctx, "project", "binding", app.Project())
	defer func() { end(err) }()
	dest := app.Destination()
	var selected *Binding
	if c.kubernetes != nil {
		binding, err := c.kubernetes.binding(ctx, app)
		if err != nil {
			return nil, err
		}
		snapshot := c.snapshot
		snapshot.Bindings = []Binding{binding}
		if err := snapshot.Validate(c.API.Instance); err != nil {
			return nil, err
		}
		selected = &binding
	}
	for i := range c.snapshot.Bindings {
		b := &c.snapshot.Bindings[i]
		if b.Project == app.Project() && b.ApplicationNamespace == app.ID.Namespace && b.RepositoryURL == domain.String(app.Source(), "repoURL") && reflect.DeepEqual(b.Destination, dest) && (b.Applications == nil || slices.Contains(b.Applications, app.ID.Name)) {
			if selected != nil {
				return nil, invalid("multiple repo-server bindings match this Application")
			}
			selected = b
		}
	}
	if selected == nil {
		return nil, denied("desired Application is outside the trusted repo-server bindings")
	}
	return selected, nil
}

// authorize checks selected against the AppProject spec project for the
// destination as Argo CD identifies it.
func (c *Client) authorize(ctx context.Context, selected *Binding, project map[string]any) (err error) {
	end := domain.TraceStart(ctx, "project", "authorize", selected.Project)
	defer func() { end(err) }()
	authorizationBinding := *selected
	authorizationBinding.Destination = domain.CloneSpec(selected.Destination)
	if selected.resolvedServer != "" {
		authorizationBinding.Destination["server"] = selected.resolvedServer
	}
	if selected.resolvedName != "" {
		authorizationBinding.Destination["name"] = selected.resolvedName
	}
	needsIdentity := false
	if destinations, ok := project["destinations"].([]any); ok {
		for _, value := range destinations {
			rule, _ := value.(map[string]any)
			for _, key := range []string{"server", "name"} {
				needsIdentity = needsIdentity || (domain.String(rule, key) != "" && domain.String(authorizationBinding.Destination, key) == "")
			}
		}
	}
	if needsIdentity {
		identity, err := c.API.ClusterCapabilities(ctx, domain.String(selected.Destination, "server"), domain.String(selected.Destination, "name"))
		if err != nil {
			return err
		}
		authorizationBinding.Destination["server"], authorizationBinding.Destination["name"] = identity.Server, identity.Name
	}
	return argopolicy.Authorize(project, authorizationBinding.RepositoryURL, authorizationBinding.Destination, authorizationBinding.ApplicationNamespace, c.snapshot.ArgoCDNamespace)
}

func invalid(reason string) error {
	return domain.Fail(domain.CodeConfigurationInvalid, "configuration", reason, "correct the trusted instance-specific repo-server configuration", nil)
}
func denied(reason string) error {
	return domain.Fail(domain.CodeAccessDenied, "permission", reason, "ask the administrator to review the exact repository, project, Application and destination binding", nil)
}
func otherInstance() error {
	return domain.Fail(domain.CodeRoutingInvalid, "render", "Application belongs to another Argo CD instance", "correct the instance routing", nil)
}
func drift(reason string) error {
	return domain.Fail(domain.CodeRenderContextChanged, "render", reason, "retry after the render context is stable", nil)
}

// The capabilities preview discovers on a repo-server backend.
var (
	_ domain.Renderer               = (*Client)(nil)
	_ domain.ProjectChecker         = (*Client)(nil)
	_ domain.DesiredRenderer        = (*Client)(nil)
	_ domain.MultiSourceRenderer    = (*Client)(nil)
	_ domain.SourceRevisionResolver = (*Client)(nil)
	_ domain.BackendInfoProvider    = (*Client)(nil)
)
