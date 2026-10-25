package preview

import (
	"context"
	"testing"

	"diffaro/internal/domain"
)

func TestExplicitRootRenderSkipKeepsIndependentResults(t *testing.T) {
	b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root"), "independent": graphApp("independent")}, render: func(_ context.Context, a domain.Application, revision string) ([]string, error) {
		if a.ID.Name == "root" {
			return nil, &domain.RenderSkipped{Reason: "Local Helm analysis skipped: destination capabilities unavailable"}
		}
		return []string{configMap(revision)}, nil
	}}
	report := graphRunner(b, "root", "independent").Run(context.Background(), graphPair())
	root := findApp(t, report, "root")
	if root.Execution != "skipped" || root.SkipReason == "" || len(root.Resources) != 0 || len(root.Issues) != 0 || report.Status != "success" || report.ExitCode() != 1 {
		t.Fatal("skipped root became no change, an empty target or a failure", report)
	}
	if findApp(t, report, "independent").Execution != "complete" {
		t.Fatal("independent comparison lost")
	}
}

func TestBasicAnalysisSkippedSubtreeDoesNotInventPruning(t *testing.T) {
	b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root"), "independent": graphApp("independent")}, render: func(_ context.Context, a domain.Application, rev string) ([]string, error) {
		switch a.ID.Name {
		case "root":
			child := graphApp("middle")
			if rev == graphHead {
				child.Source()["helm"].(map[string]any)["releaseName"] = "new-release"
			}
			return []string{appManifest(child)}, nil
		case "middle":
			if rev == graphHead {
				return nil, &domain.RenderSkipped{Reason: domain.DetailedChildAnalysisSkipReason}
			}
			return []string{appManifest(graphApp("leaf"))}, nil
		default:
			return []string{configMap(rev)}, nil
		}
	}}
	report := graphRunner(b, "root", "independent").RunRecursive(context.Background(), graphPair())
	middle, leaf := findApp(t, report, "middle"), findApp(t, report, "leaf")
	if report.ExitCode() != 1 || report.Status != "success" || len(report.Issues) != 0 || report.Graph.HeadComplete {
		t.Fatalf("analysis boundary became a failure or complete coverage: %+v", report)
	}
	if middle.State != "changed" || middle.Execution != "skipped" || len(middle.SpecChanges) == 0 || middle.HeadTarget.SkipReason == "" {
		t.Fatal("known Application configuration change was lost")
	}
	if leaf.State != "unknown" || leaf.Execution != "skipped" || leaf.SkipReason == "" || len(leaf.Resources) != 0 || leaf.Pruning != nil || len(leaf.Issues) != 0 {
		t.Fatalf("unobserved descendant became a removal or failure: %+v", leaf)
	}
	if independent := findApp(t, report, "independent"); independent.Execution != "complete" || independent.State != "changed" || len(independent.Resources) != 1 {
		t.Fatal("independent target comparison was lost")
	}
}

func TestSkippedDestinationChangeDoesNotInventServiceTargetLoss(t *testing.T) {
	b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, a domain.Application, rev string) ([]string, error) {
		if a.ID.Name == "root" {
			child := graphApp("child")
			if rev == graphHead {
				child.Spec["destination"] = map[string]any{"name": "other", "namespace": "new"}
			}
			return []string{appManifest(child), `{"apiVersion":"v1","kind":"Service","metadata":{"name":"api"},"spec":{"selector":{"app":"api"}}}`}, nil
		}
		if rev == graphHead {
			return nil, &domain.RenderSkipped{Reason: domain.DetailedChildAnalysisSkipReason}
		}
		return []string{`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api"},"spec":{"template":{"metadata":{"labels":{"app":"api"}}}}}`}, nil
	}}
	report := graphRunner(b, "root").RunRecursive(context.Background(), graphPair())
	if report.ExitCode() != 1 {
		t.Fatal("skipped child was treated as an empty head target in its old destination")
	}
}

func TestDetailedBackendFailureIsNotAnAnalysisSkip(t *testing.T) {
	for _, code := range []string{"desired_spec_unavailable", "access_denied", "repo_server_failed", "timeout", "revision_mapping_required"} {
		t.Run(code, func(t *testing.T) {
			b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, a domain.Application, _ string) ([]string, error) {
				if a.ID.Name == "root" {
					return []string{appManifest(graphApp("child"))}, nil
				}
				return nil, domain.Fail(code, "render", "detailed analysis failed", "review trusted backend", nil)
			}}
			report := graphRunner(b, "root").RunRecursive(context.Background(), graphPair())
			child := findApp(t, report, "child")
			if report.ExitCode() != 3 || child.Execution == "skipped" || child.SkipReason != "" || len(child.Issues) == 0 || child.Issues[0].Code != code {
				t.Fatal("backend failure was downgraded to an intentional skip")
			}
		})
	}
}

func TestBasicAnalysisUnchangedSkippedSpecIsUnknown(t *testing.T) {
	b := graphBackend{apps: map[string]domain.Application{"root": graphApp("root")}, render: func(_ context.Context, a domain.Application, _ string) ([]string, error) {
		if a.ID.Name == "root" {
			return []string{appManifest(graphApp("child"))}, nil
		}
		return nil, &domain.RenderSkipped{Reason: domain.DetailedChildAnalysisSkipReason}
	}}
	report := graphRunner(b, "root").RunRecursive(context.Background(), graphPair())
	child := findApp(t, report, "child")
	if report.ExitCode() != 0 || child.State != "unknown" || child.Execution != "skipped" {
		t.Fatal("unrendered unchanged spec was presented as an unchanged target")
	}
}
