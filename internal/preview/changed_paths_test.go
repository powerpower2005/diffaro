package preview

import (
	"context"
	"errors"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

const pathRepo = "https://github.com/example/charts"

func pathApp(source map[string]any) domain.Application {
	return domain.Application{ID: domain.ApplicationID{Instance: "one", Namespace: "argocd", Name: "api"}, Spec: map[string]any{"source": source}}
}

func helmSource(path string, helm map[string]any) map[string]any {
	return map[string]any{"repoURL": pathRepo, "path": path, "targetRevision": "main", "helm": helm}
}

func TestChangedPathSelection(t *testing.T) {
	charts := map[string]string{
		"charts/api/Chart.yaml":           "apiVersion: v2\nname: api\nversion: 0.1.0\ndependencies:\n  - name: lib\n    repository: file://./lib\n",
		"charts/api/lib/Chart.yaml":       "apiVersion: v2\nname: lib\nversion: 0.1.0\n",
		"charts/escape/Chart.yaml":        "apiVersion: v2\nname: escape\nversion: 0.1.0\ndependencies:\n  - name: shared\n    repository: file://../shared\n",
		"charts/nested/Chart.yaml":        "apiVersion: v2\nname: nested\nversion: 0.1.0\ndependencies:\n  - name: sub\n    repository: file://./sub\n",
		"charts/nested/sub/Chart.yaml":    "apiVersion: v2\nname: sub\nversion: 0.1.0\ndependencies:\n  - name: shared\n    repository: file://../../shared\n",
		"charts/legacy/Chart.yaml":        "apiVersion: v1\nname: legacy\nversion: 0.1.0\n",
		"charts/legacy/requirements.yaml": "dependencies:\n  - name: shared\n    repository: file://../shared\n",
		"charts/bad/Chart.yaml":           "dependencies: [",
		"charts/shared/Chart.yaml":        "apiVersion: v2\nname: shared\nversion: 0.1.0\n",
		"charts/linked/Chart.yaml":        "apiVersion: v2\nname: linked\nversion: 0.1.0\n",
		"charts/outside/Chart.yaml":       "apiVersion: v2\nname: outside\nversion: 0.1.0\ndependencies:\n  - name: x\n    repository: file://../../../x\n",
		"charts/missingdep/Chart.yaml":    "apiVersion: v2\nname: missingdep\nversion: 0.1.0\ndependencies:\n  - name: gone\n    repository: file://../gone\n",
	}
	read := func(_ context.Context, file string) ([]byte, bool, error) {
		if file == "charts/broken/Chart.yaml" {
			return nil, false, errors.New("unavailable")
		}
		data, ok := charts[file]
		return []byte(data), ok, nil
	}
	no, yes := false, true
	cases := []struct {
		name    string
		app     domain.Application
		manages *bool
		files   []string
		want    string
	}{
		{"unaffected", pathApp(helmSource("charts/api", map[string]any{"valueFiles": []any{"values.yaml", "./env/prod.yaml"}})), &no, []string{"charts/other/values.yaml", "README.md"}, selectionUnaffected},
		{"changed under path", pathApp(helmSource("charts/api", nil)), &no, []string{"charts/api/templates/x.yaml"}, selectionRendered},
		{"renamed from path", pathApp(helmSource("charts/api", nil)), &no, []string{"charts/other/x.yaml", "charts/api/x.yaml"}, selectionRendered},
		{"sibling prefix is not the path", pathApp(helmSource("charts/api", nil)), &no, []string{"charts/api-v2/values.yaml"}, selectionUnaffected},
		{"value file outside path unchanged", pathApp(helmSource("charts/api", map[string]any{"valueFiles": []any{"../common/values.yaml"}})), &no, []string{"charts/common/other.yaml"}, selectionUnaffected},
		{"value file outside path changed", pathApp(helmSource("charts/api", map[string]any{"valueFiles": []any{"values.yaml", "../common/values.yaml"}})), &no, []string{"charts/common/values.yaml"}, selectionRendered},
		{"value file escapes repository", pathApp(helmSource("charts/api", map[string]any{"valueFiles": []any{"../../../values.yaml"}})), &no, nil, selectionIneligible},
		{"absolute value file", pathApp(helmSource("charts/api", map[string]any{"valueFiles": []any{"/common/values.yaml"}})), &no, nil, selectionIneligible},
		{"variable value file", pathApp(helmSource("charts/api", map[string]any{"valueFiles": []any{"values-$ARGOCD_APP_NAME.yaml"}})), &no, nil, selectionIneligible},
		{"glob value file", pathApp(helmSource("charts/api", map[string]any{"valueFiles": []any{"values-*.yaml"}})), &no, nil, selectionIneligible},
		{"file parameter outside path changed", pathApp(helmSource("charts/api", map[string]any{"fileParameters": []any{map[string]any{"name": "x", "path": "../config.txt"}}})), &no, []string{"charts/config.txt"}, selectionRendered},
		{"file parameter escapes repository", pathApp(helmSource("charts/api", map[string]any{"fileParameters": []any{map[string]any{"name": "x", "path": "../../../secret.txt"}}})), &no, nil, selectionIneligible},
		{"file dependency outside path unchanged", pathApp(helmSource("charts/escape", nil)), &no, []string{"charts/other/values.yaml"}, selectionUnaffected},
		{"file dependency outside path changed", pathApp(helmSource("charts/escape", nil)), &no, []string{"charts/shared/templates/x.yaml"}, selectionRendered},
		{"nested dependency outside path changed", pathApp(helmSource("charts/nested", nil)), &no, []string{"charts/shared/values.yaml"}, selectionRendered},
		{"legacy requirements outside path changed", pathApp(helmSource("charts/legacy", nil)), &no, []string{"charts/shared/Chart.yaml"}, selectionRendered},
		{"legacy requirements outside path unchanged", pathApp(helmSource("charts/legacy", nil)), &no, nil, selectionUnaffected},
		{"file dependency escapes repository", pathApp(helmSource("charts/outside", nil)), &no, nil, selectionIneligible},
		{"missing dependency chart", pathApp(helmSource("charts/missingdep", nil)), &no, nil, selectionIneligible},
		{"symbolic link inside the chart", pathApp(helmSource("charts/linked", nil)), &no, nil, selectionIneligible},
		{"symbolic link elsewhere", pathApp(helmSource("charts/api", nil)), &no, []string{"docs/link"}, selectionUnaffected},
		{"invalid chart metadata", pathApp(helmSource("charts/bad", nil)), &no, nil, selectionIneligible},
		{"missing chart", pathApp(helmSource("charts/missing", nil)), &no, nil, selectionIneligible},
		{"unreadable chart", pathApp(helmSource("charts/broken", nil)), &no, nil, selectionIneligible},
		{"App-of-Apps parent", pathApp(helmSource("charts/api", nil)), &yes, nil, selectionIneligible},
		{"unknown children", pathApp(helmSource("charts/api", nil)), nil, nil, selectionIneligible},
		{"repository root", pathApp(helmSource(".", nil)), &no, nil, selectionIneligible},
		{"other repository", pathApp(map[string]any{"repoURL": "https://github.com/example/other", "path": "charts/api", "helm": map[string]any{}}), &no, nil, selectionIneligible},
		{"kustomize", pathApp(map[string]any{"repoURL": pathRepo, "path": "charts/api", "kustomize": map[string]any{}}), &no, nil, selectionIneligible},
		{"multi-source", domain.Application{ID: domain.ApplicationID{Instance: "one", Namespace: "argocd", Name: "api"}, Spec: map[string]any{"sources": []any{helmSource("charts/api", nil)}}}, &no, nil, selectionIneligible},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selector := newChangedPathSelector(ChangedPaths{Files: tc.files, Read: read, Listed: true, Links: []string{"charts/linked/templates/shared.yaml", "docs/link"}}, profile.Git{RepositoryURL: pathRepo}.Matches)
			got, reason := selector.decide(context.Background(), tc.app, tc.manages)
			if got != tc.want || reason == "" {
				t.Fatalf("selection=%s want %s (%s)", got, tc.want, reason)
			}
		})
	}
}

func TestChangedPathSelectionReadsSharedChartsOnce(t *testing.T) {
	reads := 0
	read := func(_ context.Context, file string) ([]byte, bool, error) {
		reads++
		if file == "charts/api/Chart.yaml" {
			return []byte("apiVersion: v2\nname: api\nversion: 0.1.0\n"), true, nil
		}
		return nil, false, nil
	}
	no := false
	selector := newChangedPathSelector(ChangedPaths{Read: read, Listed: true}, profile.Git{RepositoryURL: pathRepo}.Matches)
	for i := 0; i < 5; i++ {
		if got, _ := selector.decide(context.Background(), pathApp(helmSource("charts/api", nil)), &no); got != selectionUnaffected {
			t.Fatal(got)
		}
	}
	if reads != 2 {
		t.Fatalf("shared chart metadata was read %d times", reads)
	}
}

func TestChangedPathSelectionTreeListing(t *testing.T) {
	reads := []string{}
	read := func(_ context.Context, file string) ([]byte, bool, error) {
		reads = append(reads, file)
		return []byte("apiVersion: v2\nname: api\nversion: 0.1.0\n"), true, nil
	}
	no := false
	app := pathApp(helmSource("charts/api", nil))
	unlisted := newChangedPathSelector(ChangedPaths{Read: read}, profile.Git{RepositoryURL: pathRepo}.Matches)
	if got, _ := unlisted.decide(context.Background(), app, &no); got != selectionIneligible {
		t.Fatalf("links could not be excluded without a tree listing: %s", got)
	}
	reads = nil
	listed := newChangedPathSelector(ChangedPaths{Read: read, Listed: true, Present: map[string]bool{"charts/api/Chart.yaml": true}}, profile.Git{RepositoryURL: pathRepo}.Matches)
	if got, _ := listed.decide(context.Background(), app, &no); got != selectionUnaffected || len(reads) != 1 || reads[0] != "charts/api/Chart.yaml" {
		t.Fatalf("absent metadata was read: %s %v", got, reads)
	}
}

func TestChangedPathSelectionRetriesExpiredReads(t *testing.T) {
	calls := 0
	read := func(ctx context.Context, _ string) ([]byte, bool, error) {
		calls++
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return []byte("apiVersion: v2\nname: api\nversion: 0.1.0\n"), true, nil
	}
	no := false
	app := pathApp(helmSource("charts/api", nil))
	selector := newChangedPathSelector(ChangedPaths{Read: read, Listed: true}, profile.Git{RepositoryURL: pathRepo}.Matches)
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if got, _ := selector.decide(expired, app, &no); got != selectionIneligible {
		t.Fatal(got)
	}
	if got, _ := selector.decide(context.Background(), app, &no); got != selectionUnaffected {
		t.Fatalf("an expired read was cached for later candidates: %s", got)
	}
}
