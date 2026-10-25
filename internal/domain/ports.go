package domain

import "context"

type ApplicationInventory interface {
	ListApplications(context.Context, string, int) ([]OwnedApplication, error)
}

type GitProvider interface {
	Resolve(context.Context, string, string) (string, error)
	PullRequest(context.Context, string, int) (RevisionPair, error)
}

type Renderer interface {
	Application(context.Context, ApplicationID) (Application, error)
	Render(context.Context, Application, string) ([]string, error)
}

// ProjectChecker is an optional Renderer capability that verifies the
// Application's AppProject permits the comparison before rendering.
type ProjectChecker interface {
	CheckProject(context.Context, Application) error
}

// DesiredRenderer must render the supplied spec faithfully or return an error.
// It must never mutate a production Application to satisfy a render request.
type DesiredRenderer interface {
	RenderDesired(context.Context, Application, string) ([]string, error)
}

// BackendInfoProvider is an optional Renderer capability that describes the
// render backend for the report's renderBackends.
type BackendInfoProvider interface {
	BackendInfo() BackendInfo
}

type BackendVersionReader interface {
	BackendVersion(context.Context) (string, error)
}
