package localhelm

import (
	"errors"
	"testing"

	"diffaro/internal/domain"
)

func TestLinkedInputsSkipOnlyReadPaths(t *testing.T) {
	plan := []domain.SourceRevision{
		{Position: 1, Source: map[string]any{"path": "charts/app", "helm": map[string]any{"valueFiles": []any{"values.yaml", "$values/env/prod.yaml"}, "fileParameters": []any{map[string]any{"name": "config", "path": "files/config.json"}}}}},
		{Position: 2, Ref: "values", Source: map[string]any{"ref": "values"}},
	}
	var skipped *domain.RenderSkipped
	for _, tc := range []struct {
		name     string
		chart    []string
		values   []string
		wantSkip bool
	}{
		{"unrelated links", []string{".editor/hooks", "Makefile"}, []string{"docs/link"}, false},
		{"linked chart directory", []string{"charts/app"}, nil, true},
		{"link inside chart", []string{"charts/app/templates/shared.yaml"}, nil, true},
		{"chart under linked directory", []string{"charts"}, nil, true},
		{"linked value file", []string{"charts/app/values.yaml"}, nil, true},
		{"linked file parameter", []string{"charts/app/files"}, nil, true},
		{"linked referenced value file", nil, []string{"env"}, true},
	} {
		err := linkedInputs(Request{Plan: plan}, [][]string{tc.chart, tc.values})
		if got := errors.As(err, &skipped); got != tc.wantSkip {
			t.Errorf("%s: skipped=%t err=%v", tc.name, got, err)
		}
	}
}

func TestLinkedInputsResolveValuePathsLikeTheRender(t *testing.T) {
	req := fixtureRequest()
	req.Plan = []domain.SourceRevision{{Position: 1, Source: map[string]any{"path": "charts/app", "helm": map[string]any{"valueFiles": []any{"/env/prod.yaml", "values-$ARGOCD_APP_NAME.yaml"}}}}}
	var skipped *domain.RenderSkipped
	for _, tc := range []struct {
		name     string
		links    []string
		wantSkip bool
	}{
		{"root-relative value file under a link", []string{"env"}, true},
		{"expanded value file is a link", []string{"charts/app/values-api.yaml"}, true},
		{"similar unrelated link", []string{"envs", "charts/application"}, false},
	} {
		err := linkedInputs(req, [][]string{tc.links})
		if got := errors.As(err, &skipped); got != tc.wantSkip {
			t.Errorf("%s: skipped=%t err=%v", tc.name, got, err)
		}
	}
}
