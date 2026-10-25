package reporter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"diffaro/internal/domain"
	"diffaro/internal/manifest"
	"diffaro/internal/security"
)

func reviewFixture(t *testing.T) domain.Report {
	t.Helper()
	r := domain.NewReport()
	r.Status, r.ChangesFound = "partial_failure", true
	r.Revisions = domain.RevisionPair{Repository: "example/charts", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), Mode: "report-review-fixture-base-head"}
	r.Coverage = "synthetic-report-review-fixture"
	r.Limitations = []string{"PRESENTATION FIXTURE: synthetic manifests use the production manifest engine. Graph provenance and failures are supplied; no Git, Helm, Argo CD or Kubernetes was contacted. Live outcomes and custom backend equivalence are unverified."}
	r.RenderBackends = []domain.BackendInfo{{Instance: "east", Kind: "report-fixture", Verification: "presentation-fixture"}, {Instance: "west", Kind: "report-fixture", Verification: "presentation-fixture"}}
	raw := func(name, selector string) string {
		return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata: {name: %s}
spec:
  replicas: 2
  selector: {matchLabels: {app: %s}}
  template:
    metadata: {labels: {app: %s}}
    spec: {containers: [{name: api, image: example.invalid/api:1}]}
---
apiVersion: v1
kind: Service
metadata: {name: %s}
spec: {selector: {app: %s}, ports: [{port: 80, targetPort: 8080}]}
`, name, name, name, name, selector)
	}
	add := func(instance, name, cluster, state, execution, b, h string) domain.ApplicationResult {
		id := domain.ApplicationID{Instance: instance, Namespace: "argocd", Name: name}
		root := domain.ApplicationID{Instance: instance, Namespace: "argocd", Name: "platform"}
		a := domain.ApplicationResult{ID: id, Root: root, GraphPath: []domain.ApplicationID{root, id}, Cluster: cluster, Namespace: "demo", Project: "services", State: domain.State(state), Execution: domain.Execution(execution), Resources: []domain.ResourceDiff{}, Issues: []domain.Issue{}, Inputs: domain.InputSummary{Path: "charts/" + name, Origin: "parent-target-manifest", ReleaseName: name}}
		target := func(sha string) *domain.TargetSummary {
			return &domain.TargetSummary{Repository: "https://github.com/example/charts", Revision: sha, RevisionRule: "selected-repository-pair", Execution: domain.Execution(execution), Cluster: cluster, Namespace: "demo", Project: a.Project, Root: root, GraphPath: a.GraphPath, Inputs: a.Inputs}
		}
		if state != "added" {
			a.BaseTarget = target(r.Revisions.Base)
		}
		if state != "removed" {
			a.HeadTarget = target(r.Revisions.Head)
		}
		parse := func(s string) map[string]manifest.Resource {
			if s == "" {
				return map[string]manifest.Resource{}
			}
			out, err := manifest.Parse([]string{s}, instance+":"+cluster, "demo", manifest.Limits{Bytes: 1 << 20, Documents: 50}, nil)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		base, head := parse(b), parse(h)
		if execution == "complete" {
			a.Resources, _ = manifest.Compare(base, head, security.Redactor{})
		}
		return a
	}
	api := add("east", "api", "prod-east", "unchanged", "complete", raw("api", "api"), raw("api", "missing"))
	worker := add("east", "worker", "prod-east", "added", "complete", "", raw("worker", "worker")+"---\napiVersion: v1\nkind: Secret\nmetadata: {name: worker}\nstringData: {password: fictional-review-secret}\n")
	legacy := add("east", "legacy", "prod-east", "removed", "complete", raw("legacy", "legacy"), "")
	legacy.Pruning = &domain.PruningImpact{Certainty: "conditional", Reason: "base targets only; prune policy, finalizers and cascade must be checked"}
	for _, res := range legacy.Resources {
		legacy.Pruning.Resources = append(legacy.Pruning.Resources, res.ID)
	}
	denied := add("west", "api", "prod-west", "unknown", "denied", "", "")
	denied.Issues = []domain.Issue{{Code: "access_denied", ReasonCode: "request_forbidden", Stage: "render", Side: "head", Repository: "https://github.com/example/charts", Revision: r.Revisions.Head, Application: &denied.ID, Reason: "Argo CD rejected project read access", Action: "allow read-only projects/get for services"}}
	skipped := add("west", "worker", "prod-west", "added", "skipped", "", "")
	skipped.SkipReason = "Destination API capabilities are missing; allow cluster reads and refresh discovery."
	mover := add("east", "mover", "staging-east", "changed", "complete", "", "")
	mover.BaseTarget.Cluster, mover.BaseTarget.Namespace = "dev-east", "dev"
	baseMove, err := manifest.Parse([]string{raw("mover", "mover")}, "east:dev-east", "dev", manifest.Limits{Bytes: 1 << 20, Documents: 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	headMove, err := manifest.Parse([]string{raw("mover", "mover")}, "east:staging-east", "demo", manifest.Limits{Bytes: 1 << 20, Documents: 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mover.Resources, _ = manifest.Compare(baseMove, headMove, security.Redactor{})
	mover.SpecChanges = []domain.Change{{Path: "/destination/name", Operation: "replace", Before: "dev-east", After: "staging-east"}, {Path: "/destination/namespace", Operation: "replace", Before: "dev", After: "demo"}}
	r.Applications = []domain.ApplicationResult{denied, worker, legacy, api, skipped, mover}
	r.Graph = &domain.GraphSummary{BaseComplete: true, HeadComplete: false}
	for _, a := range r.Applications {
		edge := domain.GraphEdge{Parent: a.Root, Child: a.ID, Method: "fixture-supplied", Repository: "https://github.com/example/charts"}
		if a.BaseTarget != nil {
			edge.Revision = r.Revisions.Base
			r.Graph.BaseEdges = append(r.Graph.BaseEdges, edge)
		}
		if a.HeadTarget != nil {
			edge.Revision = r.Revisions.Head
			r.Graph.HeadEdges = append(r.Graph.HeadEdges, edge)
		}
	}
	return r
}

func TestReviewNavigationAnswersEnvironmentResourceActionAndCoverageQuestions(t *testing.T) {
	r := reviewFixture(t)
	original, _ := Encode(r, "json", security.Redactor{})
	for _, format := range []string{"terminal", "markdown", "pr-markdown"} {
		out, err := Encode(r, format, security.Redactor{})
		if err != nil {
			t.Fatal(err)
		}
		text := string(out)
		for _, want := range []string{"Review overview", "prod-east", "prod-west", "east/argocd/worker", "Deployment", "Service", "legacy", "2 known target candidates", "allow read-only projects/get", "Destination API capabilities are missing", "dev-east", "staging-east", "dev", "demo", "/spec/selector/app", "live deletion is unknown"} {
			if want == "2 known target candidates" && format != "terminal" {
				want = "2 resource"
				if format == "pr-markdown" {
					want = "2 candidate resource"
				}
			}
			if want == "Review overview" && format == "pr-markdown" {
				// PR Markdown groups changes by Application instead.
				continue
			}
			if !strings.Contains(text, want) {
				t.Fatalf("%s lost review answer %q", format, want)
			}
		}
		if strings.Contains(text, "fictional-review-secret") {
			t.Fatal("Secret exposed in human review")
		}
		if !strings.Contains(text, "incomplete") || !strings.Contains(text, "skipped") {
			t.Fatal("unknown coverage hidden")
		}
		if format != "terminal" {
			for _, link := range regexp.MustCompile(`\]\(#(app-[a-f0-9]+)\)`).FindAllStringSubmatch(text, -1) {
				if !strings.Contains(text, `id="`+link[1]+`"`) {
					t.Fatal("review link has no evidence anchor")
				}
			}
		}
	}
	after, _ := Encode(r, "json", security.Redactor{})
	if !bytes.Equal(original, after) {
		t.Fatal("presentation changed machine comparison")
	}
}

func TestReviewNavigationDeterministicBoundedAndControlSafe(t *testing.T) {
	r := reviewFixture(t)
	for _, format := range []string{"terminal", "markdown", "pr-markdown"} {
		first, _ := Encode(r, format, security.Redactor{})
		for i, j := 0, len(r.Applications)-1; i < j; i, j = i+1, j-1 {
			r.Applications[i], r.Applications[j] = r.Applications[j], r.Applications[i]
		}
		second, _ := Encode(r, format, security.Redactor{})
		if !bytes.Equal(first, second) {
			t.Fatal("Application input order changed navigation", format)
		}
	}
	r = domain.NewReport()
	r.Revisions.Head = strings.Repeat("b", 40)
	for i := 0; i < 150; i++ {
		r.Applications = append(r.Applications, domain.ApplicationResult{ID: domain.ApplicationID{Instance: fmt.Sprintf("i-%03d", i), Namespace: "argocd", Name: "api"}, Cluster: "prod\u202e<script>`|", State: "added", Execution: "skipped", SkipReason: strings.Repeat("가", 200) + "\nforged\x1b[2J"})
	}
	for _, format := range []string{"terminal", "pr-markdown", "markdown"} {
		out, _ := Encode(r, format, security.Redactor{})
		text := string(out)
		if !utf8.Valid(out) || strings.Contains(text, "\u202e") || (format != "terminal" && strings.Contains(text, "<script>")) || strings.Contains(text, "\x1b") {
			t.Fatal("untrusted navigation text injected display controls", format)
		}
		// PR Markdown has no overview tables; it bounds Application sections.
		if format == "pr-markdown" {
			if !strings.Contains(text, "120 more Applications with changes or incomplete results not shown here") {
				t.Fatal("Application section bound is not explicit", format)
			}
		} else if !strings.Contains(text, "142 more groups") || !strings.Contains(text, "138 more apps") {
			t.Fatal("navigation bounds are not explicit", format)
		}
		if format == "terminal" && (len(out) > MaxTerminalBytes || !strings.Contains(text, "Output shortened")) {
			t.Fatal("terminal bound lost")
		}
		if format == "pr-markdown" && len(out) > MaxPRMarkdownBytes {
			t.Fatal("PR bound lost")
		}
	}
	r = domain.NewReport()
	r.Applications = []domain.ApplicationResult{{ID: domain.ApplicationID{Name: "removed"}, State: "removed", Execution: "complete"}}
	for _, format := range []string{"terminal", "markdown", "pr-markdown"} {
		out, _ := Encode(r, format, security.Redactor{})
		if !strings.Contains(string(out), "Pruning candidates: unknown") {
			t.Fatal("missing pruning was inferred", format)
		}
	}
}

func TestReviewFixtureMachineContract(t *testing.T) {
	out, err := Encode(reviewFixture(t), "json", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	var decoded domain.Report
	if json.Unmarshal(out, &decoded) != nil || decoded.SchemaVersion != "v1alpha1" || decoded.ExitCode() != 3 {
		t.Fatal("machine/exit contract changed")
	}
}

func TestPRReviewIndexKeepsEvidenceAfterManyUnchangedApps(t *testing.T) {
	r := domain.NewReport()
	r.Revisions.Head = strings.Repeat("b", 40)
	for i := 0; i < 80; i++ {
		r.Applications = append(r.Applications, domain.ApplicationResult{ID: domain.ApplicationID{Instance: "a-stable", Namespace: "argocd", Name: fmt.Sprintf("app-%03d", i)}, Cluster: "stable", State: "unchanged", Execution: "complete"})
	}
	id := domain.ApplicationID{Instance: "z-change", Namespace: "argocd", Name: "new-api"}
	r.Applications = append(r.Applications, domain.ApplicationResult{ID: id, State: "added", Execution: "complete", Cluster: "prod", HeadTarget: &domain.TargetSummary{Repository: "https://github.com/example/charts", Revision: r.Revisions.Head, Execution: "complete", Cluster: "prod", Namespace: "demo", GraphPath: []domain.ApplicationID{id}}})
	out, err := Encode(r, "pr-markdown", security.Redactor{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("(#"+appAnchor(id)+")")) || !bytes.Contains(out, []byte(`id="`+appAnchor(id)+`"`)) || !bytes.Contains(out, []byte("Head target:")) || !bytes.Contains(out, []byte("Discovery path:")) {
		t.Fatal("unchanged prefix hid review target provenance")
	}
	before, _ := Encode(r, "json", security.Redactor{})
	_ = newReportView(r)
	after, _ := Encode(r, "json", security.Redactor{})
	if !bytes.Equal(before, after) {
		t.Fatal("review selection mutated JSON")
	}
}
