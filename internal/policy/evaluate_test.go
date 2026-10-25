package policy

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"diffaro/internal/domain"
)

func rule() domain.PolicyRule {
	return domain.PolicyRule{ID: "keep-deployments", Rule: DenyResourceRemoval, Match: domain.PolicySelector{Group: "apps", Kind: "Deployment"}}
}

func fixture() domain.Report {
	r := domain.NewReport()
	r.Revisions = domain.RevisionPair{Repository: "example/charts", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40)}
	r.ChangesFound = true
	r.Applications = []domain.ApplicationResult{{
		ID:      domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"},
		Project: "demo", State: "changed", Execution: "complete",
		Resources: []domain.ResourceDiff{{ID: domain.ResourceID{Cluster: "east:test", Group: "apps", Kind: "Deployment", Namespace: "demo", Name: "api"}, State: "removed", Comparison: "complete"}},
	}}
	return r
}

func TestTargetRemovalAndCounterexamples(t *testing.T) {
	for _, tc := range []struct {
		state       string
		count, exit int
	}{
		{"removed", 1, 2}, {"added", 0, 1}, {"changed", 0, 1}, {"unchanged", 0, 1}, {"unknown", 0, 3},
	} {
		t.Run(tc.state, func(t *testing.T) {
			r := fixture()
			r.Applications[0].Resources[0].State = domain.State(tc.state)
			Apply(&r, []domain.PolicyRule{rule()})
			if len(r.Policy.Violations) != tc.count || r.ExitCode() != tc.exit {
				t.Fatalf("unexpected policy result: %+v", r)
			}
			if tc.count > 0 {
				v := r.Policy.Violations[0]
				if !v.Evidence.Before || v.Evidence.After || v.Base.Revision != r.Revisions.Base || v.Head.Revision != r.Revisions.Head || v.PolicyID != rule().ID {
					t.Fatalf("missing evidence: %+v", v)
				}
			}
		})
	}
	r := fixture()
	Apply(&r, nil)
	if r.Policy != nil || len(r.PolicyViolations) != 0 || r.ExitCode() != 1 {
		t.Fatal("unconfigured policy changed legacy behavior")
	}
}

func TestSelectorsAndInstanceIsolation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*domain.PolicySelector)
		want   int
	}{
		{"matching", func(s *domain.PolicySelector) {
			s.Instance, s.Project, s.Namespace, s.Name = "east", "demo", "demo", "api"
		}, 1},
		{"instance", func(s *domain.PolicySelector) { s.Instance = "west" }, 0},
		{"project", func(s *domain.PolicySelector) { s.Project = "other" }, 0},
		{"group", func(s *domain.PolicySelector) { s.Group = "" }, 0},
		{"kind", func(s *domain.PolicySelector) { s.Kind = "StatefulSet" }, 0},
		{"namespace", func(s *domain.PolicySelector) { s.Namespace = "other" }, 0},
		{"name", func(s *domain.PolicySelector) { s.Name = "other" }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, p := fixture(), rule()
			tc.change(&p.Match)
			Apply(&r, []domain.PolicyRule{p})
			if len(r.Policy.Violations) != tc.want {
				t.Fatalf("selector not respected: %+v", r.Policy)
			}
		})
	}
	r := fixture()
	second := r.Applications[0]
	second.ID.Instance = "west"
	r.Applications = append(r.Applications, second)
	p := rule()
	p.Match.Instance = "east"
	Apply(&r, []domain.PolicyRule{p})
	if len(r.Policy.Violations) != 1 || r.Policy.Violations[0].Application.Instance != "east" {
		t.Fatal("same named apps conflated across instances")
	}
}

func TestUnknownBranchesAndFailurePriority(t *testing.T) {
	for _, state := range []string{"failed", "denied", "skipped", "unknown"} {
		t.Run(state, func(t *testing.T) {
			r := fixture()
			second := r.Applications[0]
			second.ID.Name, second.Execution = "incomplete", domain.Execution(state)
			r.Applications = append(r.Applications, second)
			Apply(&r, []domain.PolicyRule{rule()})
			if r.ExitCode() != 3 || r.Policy.Evaluation != "incomplete" || len(r.Policy.Violations) != 1 {
				t.Fatal("unknown branch became a removal or hid independent evidence")
			}
		})
	}
	for _, tc := range []struct {
		status string
		exit   int
	}{{"partial_failure", 3}, {"denied", 5}, {"failed", 6}, {"cancelled", 7}, {"invalid_configuration", 4}} {
		r := fixture()
		r.Status = domain.Status(tc.status)
		Apply(&r, []domain.PolicyRule{rule()})
		if r.ExitCode() != tc.exit || len(r.Policy.Violations) != 1 || r.Policy.Evaluation != "incomplete" {
			t.Fatalf("failure precedence lost for %s", tc.status)
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*domain.Report)
	}{
		{"empty", func(r *domain.Report) { r.Applications = nil }},
		{"revision", func(r *domain.Report) { r.Revisions.Head = ""; r.Applications = nil }},
		{"graph", func(r *domain.Report) {
			r.Graph = &domain.GraphSummary{BaseComplete: true}
			r.Applications[0].Execution = "failed"
		}},
		{"resource", func(r *domain.Report) { r.Applications[0].Resources[0].Comparison = "incomplete" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fixture()
			tc.change(&r)
			Apply(&r, []domain.PolicyRule{rule()})
			if r.Policy.Evaluation != "incomplete" || len(r.Policy.Violations) != 0 || r.ExitCode() != 3 {
				t.Fatalf("unknown treated as safe: %+v", r.Policy)
			}
		})
	}
}

func TestSourceProvenanceAndRemovedApplication(t *testing.T) {
	r, p := fixture(), rule()
	r.Applications[0].Project = "new-project"
	p.Match.Project = "base-project"
	base := &domain.TargetSummary{Project: "base-project", Repository: "https://git.example.invalid/other/charts", Revision: strings.Repeat("c", 40), Sources: []domain.SourceSummary{{Position: 1, Repository: "https://git.example.invalid/other/values", Revision: strings.Repeat("d", 40)}}}
	r.Applications[0].BaseTarget = base
	r.Applications[0].State = "removed"
	Apply(&r, []domain.PolicyRule{p})
	if len(r.Policy.Violations) != 1 {
		t.Fatal("policy must use base project for removed resource")
	}
	v := r.Policy.Violations[0]
	if v.Base.Revision != base.Revision || !reflect.DeepEqual(v.Base.Sources, base.Sources) || !v.Head.Absent || v.Head.Revision != "" {
		t.Fatalf("source provenance lost or absent revision invented: %+v", v)
	}
}

func TestDeterministicBounds(t *testing.T) {
	r := fixture()
	for i := 0; i < MaxViolations+10; i++ {
		res := r.Applications[0].Resources[0]
		res.ID.Name = fmt.Sprintf("deployment-%04d", i)
		r.Applications[0].Resources = append(r.Applications[0].Resources, res)
	}
	other := r
	other.Applications = append([]domain.ApplicationResult{}, r.Applications...)
	other.Applications[0].Resources = append([]domain.ResourceDiff{}, r.Applications[0].Resources...)
	for left, right := 0, len(other.Applications[0].Resources)-1; left < right; left, right = left+1, right-1 {
		other.Applications[0].Resources[left], other.Applications[0].Resources[right] = other.Applications[0].Resources[right], other.Applications[0].Resources[left]
	}
	Apply(&r, []domain.PolicyRule{rule()})
	Apply(&other, []domain.PolicyRule{rule()})
	a, _ := json.Marshal(r.Policy)
	b, _ := json.Marshal(other.Policy)
	if string(a) != string(b) || len(r.Policy.Violations) != MaxViolations || r.Policy.Evaluation != "incomplete" || r.ExitCode() != 3 {
		t.Fatal("bounded output is nondeterministic or claims complete evaluation")
	}
	r = fixture()
	res := r.Applications[0].Resources[0]
	res.State = "unchanged"
	r.Applications[0].Resources = make([]domain.ResourceDiff, MaxResources+1)
	for i := range r.Applications[0].Resources {
		r.Applications[0].Resources[i] = res
	}
	Apply(&r, []domain.PolicyRule{rule()})
	if r.Policy.Evaluation != "incomplete" || r.ExitCode() != 3 {
		t.Fatal("resource limit silently ignored")
	}
}

func TestEvidenceByteLimitPreservesIndependentFindings(t *testing.T) {
	r := fixture()
	large := r.Applications[0]
	large.ID.Name = "oversized"
	large.BaseSources = []domain.SourceSummary{{Position: 1, Repository: strings.Repeat("x", MaxEvidenceBytes), Revision: r.Revisions.Base}}
	r.Applications = append(r.Applications, large)
	Apply(&r, []domain.PolicyRule{rule()})
	if r.ExitCode() != 3 || r.Policy.Evaluation != "incomplete" || len(r.Policy.Violations) != 1 || r.Policy.Violations[0].Application.Name != "api" {
		t.Fatal("oversized evidence hid an independent finding or claimed complete coverage")
	}
	if !strings.Contains(strings.Join(r.Policy.Limitations, " "), "evidence byte limit") {
		t.Fatal("missing actionable evidence limit reason")
	}
	encoded, err := json.Marshal(r.Policy)
	if err != nil || len(encoded) > MaxEvidenceBytes {
		t.Fatal("oversized source provenance escaped the evidence bound")
	}
}

func TestValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*domain.PolicyRule)
	}{
		{"empty ID", func(p *domain.PolicyRule) { p.ID = "" }},
		{"long ID", func(p *domain.PolicyRule) { p.ID = strings.Repeat("a", 65) }},
		{"unsupported rule", func(p *domain.PolicyRule) { p.Rule = "script" }},
		{"missing kind", func(p *domain.PolicyRule) { p.Match.Kind = "" }},
		{"wildcard", func(p *domain.PolicyRule) { p.Match.Namespace = "*" }},
		{"control", func(p *domain.PolicyRule) { p.Match.Name = "secret\nvalue" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := rule()
			tc.change(&p)
			if Validate([]domain.PolicyRule{p}) == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
	if Validate([]domain.PolicyRule{rule()}) != nil || Validate(nil) != nil {
		t.Fatal("valid policy rejected")
	}
	if Validate([]domain.PolicyRule{rule(), rule()}) == nil || Validate(make([]domain.PolicyRule, MaxRules+1)) == nil {
		t.Fatal("duplicate or oversized policies accepted")
	}
}
