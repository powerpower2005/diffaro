package revision

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

func TestPlanSourcesPreservesInputsAndRevisionPositions(t *testing.T) {
	base, head, chart := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	app := domain.Application{Spec: map[string]any{"sources": []any{
		map[string]any{"repoURL": "https://github.com/example/chart.git", "targetRevision": chart, "path": "chart", "helm": map[string]any{"valueFiles": []any{"$values/prod.yaml"}, "releaseName": "shop"}},
		map[string]any{"repoURL": "git@github.com:example/values.git", "targetRevision": "main", "ref": "values"},
		map[string]any{"repoURL": "https://github.com/example/values.git", "targetRevision": "main", "ref": "extra"},
	}}}
	before := domain.CloneSpec(app.Spec)
	for _, sha := range []string{base, head} {
		plan, err := PlanSources(context.Background(), app, "https://github.com/example/values", sha, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan) != 3 || plan[0].Revision != chart || plan[1].Revision != sha || plan[2].Revision != sha || plan[1].Position != 2 || plan[1].Ref != "values" {
			t.Fatal("source mapping or ordering lost")
		}
		if !reflect.DeepEqual(plan[0].Source["helm"], before["sources"].([]any)[0].(map[string]any)["helm"]) {
			t.Fatal("Helm inputs changed")
		}
	}
	if !reflect.DeepEqual(before, app.Spec) {
		t.Fatal("stored input mutated")
	}
}

func TestPlanSourcesRejectsAmbiguousAndUnresolvedInputs(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, sources := range []any{"invalid", []any{nil}, []any{map[string]any{"repoURL": "https://example.com/x/y", "targetRevision": "main"}}, []any{map[string]any{"repoURL": "x", "ref": "values", "targetRevision": sha}, map[string]any{"repoURL": "y", "ref": "values", "targetRevision": sha}}} {
		if _, err := PlanSources(context.Background(), domain.Application{Spec: map[string]any{"sources": sources}}, "selected", sha, nil); err == nil {
			t.Fatal("invalid source plan accepted")
		}
	}
}

func TestPlanSourcesResolver(t *testing.T) {
	sha := strings.Repeat("a", 40)
	app := domain.Application{Spec: map[string]any{"source": map[string]any{"repoURL": "https://example.com/other/repo", "targetRevision": "stable"}}}
	plan, err := PlanSources(context.Background(), app, "selected", sha, func(_ context.Context, repo, ref string) (string, error) {
		if repo != "https://example.com/other/repo" || ref != "stable" {
			t.Fatal("resolver input changed")
		}
		return sha, nil
	})
	if err != nil || plan[0].Rule != "resolved-source-reference" || plan[0].Requested != "stable" || plan[0].Revision != sha {
		t.Fatal("resolved source or its requested reference was not recorded", plan)
	}
}

func TestNonPRTagPrefixCannotReuseAnUnqualifiedResolution(t *testing.T) {
	sha := strings.Repeat("a", 40)
	source := map[string]any{"repoURL": "https://example.com/chart", "targetRevision": "1.*", "tagPrefix": "component/"}
	app := domain.Application{Spec: map[string]any{"sources": []any{source}}}
	resolver := func(context.Context, string, string) (string, error) {
		t.Fatal("tagPrefix was omitted from revision resolution")
		return "", nil
	}
	if _, err := PlanSources(context.Background(), app, "https://example.com/values", sha, resolver); err == nil {
		t.Fatal("ambiguous prefixed source was accepted")
	}
	source["targetRevision"] = sha
	if _, err := PlanSources(context.Background(), app, "https://example.com/values", sha, resolver); err != nil {
		t.Fatal("explicitly pinned source was rejected", err)
	}
}

func TestPlanSourcesUsesExplicitRepositoryAliases(t *testing.T) {
	sha, chart := strings.Repeat("a", 40), strings.Repeat("c", 40)
	app := domain.Application{Spec: map[string]any{"sources": []any{
		map[string]any{"repoURL": "https://mirror.example.com/example/charts", "targetRevision": "main", "path": "chart", "helm": map[string]any{}},
		map[string]any{"repoURL": "https://mirror.example.com/example/other", "targetRevision": chart, "ref": "values"},
	}}}
	plan, err := PlanSources(context.Background(), app, "https://github.com/example/charts", sha, nil, "https://mirror.example.com/example/charts")
	if err != nil || plan[0].Revision != sha || plan[0].Rule != "selected-repository-pair" || plan[1].Revision != chart {
		t.Fatal("alias source did not use the selected revision, or another repository was remapped", err)
	}
	if plan[0].Repository != "https://mirror.example.com/example/charts" {
		t.Fatal("stored source URL was rewritten")
	}
	// Without the alias the mirror is a different repository with a mutable ref.
	if _, err := PlanSources(context.Background(), app, "https://github.com/example/charts", sha, nil); err == nil {
		t.Fatal("mirror source was treated as the selected repository without an alias")
	}
}
