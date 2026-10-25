package argocd

import (
	"context"
	"errors"
	"net/url"

	"diffaro/internal/domain"
)

func projectAccess(name string) *domain.AccessRequirement {
	return &domain.AccessRequirement{System: "argocd-rbac", Resource: "projects", Action: "get", Target: name, Object: name, Scope: "this AppProject; attach the grant to the account/role of this credential"}
}

func applicationAccess(id domain.ApplicationID, project string) *domain.AccessRequirement {
	target := "namespace=" + id.Namespace + "; application=" + id.Name
	scope := "AppProject is unresolved; ask the administrator for Application.spec.project and the server's Application namespace policy"
	if project != "" {
		target = "project=" + project + "; " + target
		scope = "confirm the server's namespace policy: object=" + project + "/" + id.Name + " in the control-plane namespace, or " + project + "/" + id.Namespace + "/" + id.Name + " for Applications in other enabled namespaces"
	}
	return &domain.AccessRequirement{System: "argocd-rbac", Resource: "applications", Action: "get", Target: target, Scope: scope}
}

func inventoryAccess(namespace string) *domain.AccessRequirement {
	target := "selected RBAC-visible Applications"
	if namespace != "" {
		target += "; namespace=" + namespace
	}
	return &domain.AccessRequirement{System: "argocd-rbac", Resource: "applications", Action: "get", Target: target, Scope: "grant access to the selected Application objects; inventory does not require granting all projects or all Applications"}
}

func clusterAccess(kind, value string) *domain.AccessRequirement {
	// A destination is not yet an authenticated cluster registration. In
	// particular, its name is not a cluster RBAC object, and a server may carry
	// a registration project scope. Do not invent an exact policy object.
	if kind == "server" {
		parsed, err := url.Parse(value)
		if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			value = "[unsafe destination identifier omitted]"
		}
	}
	return &domain.AccessRequirement{System: "argocd-rbac", Resource: "clusters", Action: "get", Target: kind + "=" + value, Scope: "exact RBAC object is unresolved: confirm the registered server and any registration project scope; the cluster name is not the RBAC object"}
}

func apiAccess(route string) *domain.AccessRequirement {
	return &domain.AccessRequirement{System: "argocd-api", Action: "GET", Target: route, Scope: "endpoint access/authentication; no Argo CD RBAC resource grant is inferred"}
}

// KnownApplicationAccess restates a denied Application read with app's known
// AppProject. Every backend applies it to its Application reads around a render.
func KnownApplicationAccess(err error, app domain.Application) error {
	var failure *domain.Failure
	if !errors.As(err, &failure) || failure.Issue.RequiredAccess == nil || failure.Issue.RequiredAccess.Resource != "applications" {
		return err
	}
	copy := *failure
	required := applicationAccess(app.ID, app.Project())
	required.Instance = failure.Issue.RequiredAccess.Instance
	required.CredentialEnv = failure.Issue.RequiredAccess.CredentialEnv
	copy.Issue.RequiredAccess = required
	return &copy
}

// EffectiveTrackingMethod reads authenticated API settings for the optional
// Kubernetes context collector, with the same credential diagnostics as other
// Argo CD requests. It does not invent a "settings" RBAC resource.
func (c *Client) EffectiveTrackingMethod(ctx context.Context) (string, error) {
	settings, err := c.readSettings(ctx, "effective tracking settings lookup")
	if err != nil {
		return "", err
	}
	return settings.TrackingMethod, nil
}

// RenderSettings is the authenticated, non-secret subset used by local rendering.
type RenderSettings struct {
	AppLabelKey         string `json:"appLabelKey"`
	TrackingMethod      string `json:"trackingMethod"`
	InstallationID      string `json:"installationID"`
	ControllerNamespace string `json:"controllerNamespace"`
}

func (c *Client) RenderSettings(ctx context.Context) (RenderSettings, error) {
	return c.readSettings(ctx, "local render settings lookup")
}

// readSettings reads the API settings once per run for every backend of this
// instance; operation names the caller's lookup when the read fails.
func (c *Client) readSettings(ctx context.Context, operation string) (RenderSettings, error) {
	c.mu.Lock()
	cached := c.settings
	c.mu.Unlock()
	if cached != nil {
		return *cached, nil
	}
	var settings RenderSettings
	if err := c.get(ctx, "/api/v1/settings", nil, &settings, operation, "/api/v1/settings", "settings API access", apiAccess("/api/v1/settings")); err != nil {
		return settings, err
	}
	c.mu.Lock()
	c.settings = &settings
	c.mu.Unlock()
	return settings, nil
}
