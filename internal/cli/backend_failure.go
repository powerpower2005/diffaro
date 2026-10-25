package cli

import (
	"context"

	"diffaro/internal/domain"
)

// A configured detailed backend that failed preparation must never silently
// fall back to stored-spec rendering. Other instances remain independent.
type unavailableBackend struct {
	instance string
	kind     string // domain.BackendLocalHelm or domain.BackendRepoServer
	cause    error
}

func (b unavailableBackend) Application(_ context.Context, id domain.ApplicationID) (domain.Application, error) {
	return domain.Application{ID: id}, b.cause
}
func (b unavailableBackend) CheckProject(context.Context, domain.Application) error { return b.cause }
func (b unavailableBackend) Render(context.Context, domain.Application, string) ([]string, error) {
	return nil, b.cause
}
func (b unavailableBackend) RenderDesired(context.Context, domain.Application, string) ([]string, error) {
	return nil, b.cause
}
func (b unavailableBackend) RenderSources(context.Context, domain.Application, []domain.SourceRevision) ([]string, error) {
	return nil, b.cause
}
func (b unavailableBackend) RenderDesiredSources(context.Context, domain.Application, []domain.SourceRevision) ([]string, error) {
	return nil, b.cause
}
func (b unavailableBackend) ResolveSourceRevision(context.Context, domain.Application, string, string) (string, error) {
	return "", b.cause
}
func (b unavailableBackend) BackendInfo() domain.BackendInfo {
	if b.kind == domain.BackendLocalHelm {
		return domain.BackendInfo{Instance: b.instance, Kind: b.kind, Verification: "local-context-unverified", Compatibility: "setup-failed"}
	}
	return domain.BackendInfo{Instance: b.instance, Kind: b.kind, Verification: "live-unverified"}
}
