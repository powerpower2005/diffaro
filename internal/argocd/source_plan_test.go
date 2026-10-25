package argocd

import (
	"context"
	"errors"
	"testing"

	"diffaro/internal/domain"
)

// A plan that does not match the Application sources is the same failure the
// repo-server and local Helm backends report; it must not be classified as a
// revision (invalid_configuration) failure and it must fail before any request.
func TestRenderSourcesRejectsMismatchedPlanAsSourceInvalid(t *testing.T) {
	app := domain.Application{ID: domain.ApplicationID{Instance: "lab", Namespace: "argocd", Name: "api"}, Spec: map[string]any{"sources": []any{
		map[string]any{"repoURL": "https://git.example.test/charts.git", "path": "chart", "targetRevision": "main"},
	}}}
	for name, plan := range map[string][]domain.SourceRevision{
		"length":   nil,
		"position": {{Position: 2, Source: map[string]any{"repoURL": "https://git.example.test/charts.git", "path": "chart"}, Repository: "example/charts", Revision: "0123456789abcdef0123456789abcdef01234567"}},
	} {
		_, err := (&Client{}).RenderSources(context.Background(), app, plan)
		var failure *domain.Failure
		if !errors.As(err, &failure) || failure.Issue.Code != "source_invalid" || failure.Issue.Stage != "render" {
			t.Fatalf("%s: got %v", name, err)
		}
	}
}
