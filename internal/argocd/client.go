// Package argocd is the read-only adapter for the Argo CD Application API.
//
// It looks up Applications and AppProjects, renders stored or desired specs
// through the manifests endpoint (Client.Render, RenderDesired, RenderSources),
// lists inventories for repository discovery and reads render settings and
// cluster capabilities used by local Helm and repo-server. Every backend
// renders a stored Application through Client.Bracket, between two reads of
// its spec; local Helm and repo-server also re-read the AppProject after a
// render (ProjectUnchanged). A change fails as render_context_changed. It
// never syncs, patches or deletes.
package argocd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"sync"

	"diffaro/internal/domain"
	"diffaro/internal/helmerror"
	"diffaro/internal/httpx"
)

type Client struct {
	HTTP     *httpx.Client
	Instance string
	// mu guards the run-scoped reads below, shared by every backend of this
	// instance. Only successful reads are kept; a failed read is repeated by
	// the next caller.
	mu           sync.Mutex
	settings     *RenderSettings
	clusters     map[string]ClusterCapabilities
	clusterBytes int
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any, operation, route, permission string, required *domain.AccessRequirement) error {
	return c.HTTP.GetFor(ctx, path, query, out, httpx.RequestDiagnostic{
		Service: "Argo CD", Instance: c.Instance, Operation: operation, Route: route, Permission: permission,
		RequiredAccess:       required,
		AuthenticationAction: "confirm the token belongs to this Argo CD instance; confirm the account/project role grants the requested read access",
	})
}

func (c *Client) BackendInfo() domain.BackendInfo {
	return domain.BackendInfo{Instance: c.Instance, Kind: "argocd-application-api", ContextID: "stored-application-spec", Verification: "live-unverified"}
}

func (c *Client) BackendVersion(ctx context.Context) (string, error) {
	var response struct {
		Version string `json:"Version"`
	}
	if err := c.get(ctx, "/api/version", nil, &response, "version lookup", "/api/version", "version API access", apiAccess("/api/version")); err != nil {
		return "", err
	}
	if response.Version == "" {
		return "", domain.Fail(domain.CodeResponseInvalid, "capabilities", "backend version is missing", "check Argo CD version API compatibility", nil)
	}
	return response.Version, nil
}

func (c *Client) Application(ctx context.Context, id domain.ApplicationID) (domain.Application, error) {
	return c.application(ctx, id, "")
}

// desiredApplication reads the stored Application for a desired child spec.
// Without a project, Argo CD answers a missing Application with 403 so RBAC
// does not reveal existence; scoping the query to the desired project lets a
// newly added child read as not found. A stored Application in another
// project denies the scoped query, so the unscoped read keeps that spec
// difference observable.
func (c *Client) desiredApplication(ctx context.Context, desired domain.Application) (domain.Application, error) {
	project := desired.Project()
	stored, err := c.application(ctx, desired.ID, project)
	var failure *domain.Failure
	if project == "" || !errors.As(err, &failure) || failure.Issue.Code != domain.CodeAccessDenied {
		return stored, err
	}
	if unscoped, retry := c.application(ctx, desired.ID, ""); retry == nil {
		return unscoped, nil
	}
	return stored, err
}

func (c *Client) application(ctx context.Context, id domain.ApplicationID, project string) (domain.Application, error) {
	if id.Instance != c.Instance {
		return domain.Application{}, domain.Fail(domain.CodeRoutingInvalid, "lookup", "Application belongs to another Argo CD instance", "correct the instance routing", nil)
	}
	var response struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec   map[string]any `json:"spec"`
		Status struct {
			Resources []statusResource `json:"resources"`
		} `json:"status"`
	}
	query := url.Values{"appNamespace": {id.Namespace}}
	if project != "" {
		query.Set("project", project)
	}
	if err := c.get(ctx, "/api/v1/applications/"+url.PathEscape(id.Name), query, &response, "Application lookup", "/api/v1/applications/{application}", "applications/get for the target project/Application (include Application namespace when enabled)", applicationAccess(id, project)); err != nil {
		return domain.Application{}, applicationFailure(err, id, project)
	}
	if response.Metadata.Name != id.Name || response.Metadata.Namespace != id.Namespace || response.Spec == nil {
		return domain.Application{}, domain.Fail(domain.CodeResponseInvalid, "lookup", "backend returned a different or incomplete Application identity", "check Argo CD namespace and API compatibility", nil)
	}
	return domain.Application{ID: id, Spec: response.Spec, ObservedScopes: observedScopes(response.Status.Resources)}, nil
}

type statusResource struct {
	Group     string `json:"group"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
}

// observedScopes derives group/kind scope from Argo CD's stored comparison:
// the controller sets a namespace only on namespaced resources. Kinds observed
// with both forms are omitted so the parser keeps them unknown.
func observedScopes(resources []statusResource) map[string]bool {
	scopes := map[string]bool{}
	conflicts := map[string]bool{}
	for _, r := range resources {
		if r.Kind == "" {
			continue
		}
		key := r.Group + "/" + r.Kind
		clusterScoped := r.Namespace == ""
		if previous, seen := scopes[key]; seen && previous != clusterScoped {
			conflicts[key] = true
		}
		scopes[key] = clusterScoped
	}
	for key := range conflicts {
		delete(scopes, key)
	}
	if len(scopes) == 0 {
		return nil
	}
	return scopes
}

func (c *Client) CheckProject(ctx context.Context, a domain.Application) error {
	if a.Project() == "" {
		return domain.Fail(domain.CodeResponseInvalid, "permission", "Application project is missing", "set an explicit Application project", nil)
	}
	var project json.RawMessage
	return applicationFailure(c.get(ctx, "/api/v1/projects/"+url.PathEscape(a.Project()), nil, &project, "AppProject lookup", "/api/v1/projects/{project}", "projects/get for the Application's AppProject", projectAccess(a.Project())), a.ID, a.Project())
}

func (c *Client) ProjectSpec(ctx context.Context, name string) (map[string]any, error) {
	var project struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec map[string]any `json:"spec"`
	}
	if err := c.get(ctx, "/api/v1/projects/"+url.PathEscape(name), nil, &project, "AppProject lookup", "/api/v1/projects/{project}", "projects/get for the requested AppProject", projectAccess(name)); err != nil {
		return nil, err
	}
	if project.Metadata.Name != name || project.Spec == nil {
		return nil, domain.Fail(domain.CodeResponseInvalid, "permission", "project identity or spec is missing", "check Argo CD project API compatibility", nil)
	}
	return project.Spec, nil
}

func (c *Client) Render(ctx context.Context, a domain.Application, revision string) ([]string, error) {
	return c.renderQuery(ctx, a, renderRevision(a, revision), false)
}

func renderRevision(a domain.Application, revision string) url.Values {
	return url.Values{"appNamespace": {a.ID.Namespace}, "project": {a.Project()}, "revision": {revision}}
}

// renderQuery brackets the manifest request with Application reads so a spec
// change during rendering is drift. fetched reports that a was itself read from
// Argo CD by the caller before this request; that read is the "before"
// snapshot, so it is not repeated.
func (c *Client) renderQuery(ctx context.Context, a domain.Application, q url.Values, fetched bool) ([]string, error) {
	var sourceType string
	render := func() ([]string, error) {
		var response struct {
			Manifests  []string `json:"manifests"`
			SourceType string   `json:"sourceType"`
		}
		if err := c.get(ctx, "/api/v1/applications/"+url.PathEscape(a.ID.Name)+"/manifests", q, &response, "target manifest rendering", "/api/v1/applications/{application}/manifests", "applications/get for the target project/Application", applicationAccess(a.ID, a.Project())); err != nil {
			return nil, c.renderFailure(err, a)
		}
		sourceType = response.SourceType
		return response.Manifests, nil
	}
	var manifests []string
	var err error
	if fetched {
		manifests, err = c.renderThenCheck(ctx, a, contextChanged(), render)
	} else {
		manifests, err = c.Bracket(ctx, a, contextChanged(), contextChanged(), render)
	}
	if err != nil {
		return nil, err
	}
	// GetManifests in v3.5.1 aggregates manifests without forwarding SourceType.
	// The caller requires an explicit Helm source before invoking this backend.
	if sourceType != "" && sourceType != "Helm" {
		return nil, domain.Fail(domain.CodeUnsupportedSource, "render", "backend did not identify this source as Helm", "use a Git-hosted Helm chart; other source types need a verified adapter", nil)
	}
	if manifests == nil {
		manifests = []string{}
	}
	return manifests, nil
}

// Bracket runs render between two reads of app's stored Application, so a
// spec change during rendering is drift: a read that differs from app.Spec
// fails with changedBefore or changedDuring. A partial render is bracketed
// like a complete one and returned with its error. Every backend renders a
// stored Application through it; denied reads name app's known AppProject.
func (c *Client) Bracket(ctx context.Context, app domain.Application, changedBefore, changedDuring error, render func() ([]string, error)) ([]string, error) {
	if err := c.specUnchanged(ctx, app, "drift-before", changedBefore); err != nil {
		return nil, err
	}
	return c.renderThenCheck(ctx, app, changedDuring, render)
}

// renderThenCheck is Bracket without the first read, for a caller whose app
// was itself just read from Argo CD.
func (c *Client) renderThenCheck(ctx context.Context, app domain.Application, changed error, render func() ([]string, error)) ([]string, error) {
	result, renderErr := render()
	if renderErr != nil && !domain.IsRenderPartial(renderErr) {
		return nil, renderErr
	}
	if err := c.specUnchanged(ctx, app, "drift-after", changed); err != nil {
		return nil, err
	}
	return result, renderErr
}

// specUnchanged reads app's stored Application as a traced lookup step and
// returns changed when its spec differs from app.Spec.
func (c *Client) specUnchanged(ctx context.Context, app domain.Application, step string, changed error) error {
	end := domain.TraceRead(ctx, "lookup", step, app.ID.String())
	current, err := c.Application(ctx, app.ID)
	err = KnownApplicationAccess(err, app)
	if err == nil && !reflect.DeepEqual(app.Spec, current.Spec) {
		err = changed
	}
	end(func() string { return domain.DigestJSON(current.Spec) }, err)
	return err
}

// ProjectUnchanged re-reads the AppProject name after rendering, as a traced
// project step, and returns changed when its spec differs from before, the
// spec the render was authorized against.
func (c *Client) ProjectUnchanged(ctx context.Context, name string, before map[string]any, changed error) error {
	end := domain.TraceRead(ctx, "project", "drift-after", name)
	after, err := c.ProjectSpec(ctx, name)
	if err == nil && !reflect.DeepEqual(before, after) {
		err = changed
	}
	end(func() string { return domain.DigestJSON(after) }, err)
	return err
}

func (c *Client) RenderDesired(ctx context.Context, desired domain.Application, revision string) ([]string, error) {
	stored, err := c.desiredApplication(ctx, desired)
	if err != nil {
		var failure *domain.Failure
		if errors.As(err, &failure) && failure.Issue.Code == domain.CodeNotFound {
			return nil, desiredUnavailable(unreadableChildReason, err)
		}
		return nil, KnownApplicationAccess(err, desired)
	}
	if !reflect.DeepEqual(withoutRevision(desired.Spec), withoutRevision(stored.Spec)) {
		return nil, desiredUnavailable(specMismatchReason(withoutRevision(desired.Spec), withoutRevision(stored.Spec)), nil)
	}
	if err = c.CheckProject(ctx, desired); err != nil {
		return nil, err
	}
	return c.renderQuery(ctx, stored, renderRevision(stored, revision), true)
}

func withoutRevision(spec map[string]any) map[string]any {
	copy := domain.CloneSpec(spec)
	if source, ok := copy["source"].(map[string]any); ok {
		delete(source, "targetRevision")
	}
	return copy
}

func desiredUnavailable(reason string, cause error) error {
	return &domain.RenderSkipped{Reason: reason + " " + domain.DetailedChildAnalysisSkipReason, Cause: domain.Fail(domain.CodeDesiredSpecUnavailable, "render", reason, "for optional detailed analysis, select local Helm for supported Git Helm children or configure a trusted repo-server backend; do not substitute the installed spec", cause)}
}

// renderFailure names an Argo CD render failure by the cause in its
// server message, with the reason codes repo-server and local Helm give the
// same cause, instead of a generic server error that may
// succeed on retry. A missing app path stays not_found. The message itself
// is withheld because Helm template errors can contain values; a server
// error without a known render cause stays a backend error.
func (c *Client) renderFailure(err error, a domain.Application) error {
	var failure *domain.Failure
	var status *httpx.StatusError
	if !errors.As(err, &failure) || !errors.As(err, &status) {
		return err
	}
	reasonCode, reason, action, ok := helmerror.RenderFailureCause(status.Message, "Argo CD")
	if !ok {
		return err
	}
	copy := *failure
	copy.Issue.Code, copy.Issue.ReasonCode, copy.Issue.Stage = domain.CodeRenderFailed, reasonCode, "render"
	copy.Issue.Reason = fmt.Sprintf("Argo CD instance %s target manifest rendering: %s (HTTP %d)", c.Instance, reason, status.StatusCode)
	copy.Issue.Action = action
	if reasonCode == domain.ReasonChartPathMissing {
		path := "a source path"
		if p := domain.String(a.Source(), "path"); p != "" {
			path = "source path " + strconv.Quote(p)
		}
		copy.Issue.Code = domain.CodeNotFound
		copy.Issue.Reason = fmt.Sprintf("Argo CD instance %s target manifest rendering: %s does not exist at this revision (HTTP %d)", c.Instance, path, status.StatusCode)
		copy.Issue.Action = "check the Application source path and targetRevision; Argo CD reports \"app path does not exist\" for the same revision"
	}
	return &copy
}

func contextChanged() error {
	return domain.Fail(domain.CodeRenderContextChanged, "render", "Application spec changed during preview", "retry after the Application configuration is stable", nil)
}
