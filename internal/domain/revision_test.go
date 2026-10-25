package domain

import "testing"

func TestIsCommitSHA(t *testing.T) {
	for value, want := range map[string]bool{
		"0123456789abcdef0123456789abcdef01234567":  true,
		"0123456789ABCDEF0123456789ABCDEF01234567":  true,
		"0123456789abcdef0123456789abcdef0123456":   false,
		"0123456789abcdef0123456789abcdef012345678": false,
		"main": false,
		"":     false,
		"g123456789abcdef0123456789abcdef01234567": false,
	} {
		if got := IsCommitSHA(value); got != want {
			t.Errorf("IsCommitSHA(%q) = %t, want %t", value, got, want)
		}
	}
}

func TestSourceRevisionMatchesOnlyRevisionChanges(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	original := map[string]any{"repoURL": "https://git.example/charts.git", "ref": "values", "targetRevision": "main", "path": "chart"}
	planned := func(change func(*SourceRevision)) SourceRevision {
		source := CloneSpec(original)
		source["targetRevision"] = sha
		s := SourceRevision{Position: 1, Repository: "https://git.example/charts.git", Ref: "values", Revision: sha, Source: source}
		change(&s)
		return s
	}
	for name, tc := range map[string]struct {
		plan  SourceRevision
		index int
		want  bool
	}{
		"revision only":     {planned(func(*SourceRevision) {}), 0, true},
		"wrong position":    {planned(func(*SourceRevision) {}), 1, false},
		"mutable revision":  {planned(func(s *SourceRevision) { s.Revision, s.Source["targetRevision"] = "main", "main" }), 0, false},
		"revision mismatch": {planned(func(s *SourceRevision) { s.Source["targetRevision"] = "fedcba9876543210fedcba9876543210fedcba98" }), 0, false},
		"changed input":     {planned(func(s *SourceRevision) { s.Source["path"] = "other" }), 0, false},
		"different ref":     {planned(func(s *SourceRevision) { s.Ref = "other" }), 0, false},
		"different repo":    {planned(func(s *SourceRevision) { s.Repository = "https://git.example/other.git" }), 0, false},
	} {
		if got := tc.plan.Matches(original, tc.index); got != tc.want {
			t.Errorf("%s: Matches = %t, want %t", name, got, tc.want)
		}
	}
	if (SourceRevision{}).Matches("not a source", 0) {
		t.Error("non-map source matched")
	}
}

func TestIgnoreRulesAffectTargets(t *testing.T) {
	rules := []any{map[string]any{"kind": "Secret", "jsonPointers": []any{"/data"}}}
	for name, tc := range map[string]struct {
		spec  map[string]any
		count int
		want  bool
	}{
		"no rules":                {map[string]any{}, 0, false},
		"rules only affect drift": {map[string]any{"ignoreDifferences": rules}, 1, false},
		"other sync options":      {map[string]any{"ignoreDifferences": rules, "syncPolicy": map[string]any{"syncOptions": []any{"ServerSideApply=true"}}}, 1, false},
		"respected on sync":       {map[string]any{"ignoreDifferences": rules, "syncPolicy": map[string]any{"syncOptions": []any{"CreateNamespace=true", "RespectIgnoreDifferences=true"}}}, 1, true},
		"respected without rules": {map[string]any{"syncPolicy": map[string]any{"syncOptions": []any{"RespectIgnoreDifferences=true"}}}, 0, false},
	} {
		app := Application{Spec: tc.spec}
		if app.IgnoreRuleCount() != tc.count || app.IgnoreRulesAffectTargets() != tc.want {
			t.Errorf("%s: count=%d affects=%t", name, app.IgnoreRuleCount(), app.IgnoreRulesAffectTargets())
		}
	}
}
