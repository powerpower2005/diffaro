package preview

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"diffaro/internal/domain"
	"diffaro/internal/policy"
	"diffaro/internal/profile"
)

// recursiveReport is the report of a recursive run without roots.
func recursiveReport(instances []profile.Instance, texts BackendLimitations) domain.Report {
	return Runner{Config: profile.Config{Instances: instances}, BackendLimitations: texts}.RunRecursive(context.Background(), domain.RevisionPair{})
}

func TestFinalizeStatesLocalHelmBoundariesFromInstances(t *testing.T) {
	for _, tc := range []struct {
		name        string
		instances   []profile.Instance
		local, env  bool
		description string
	}{
		{"application API only", []profile.Instance{{ID: "east"}}, false, false, "keeps the basic analysis text and adds no local Helm boundary"},
		{"local Helm", []profile.Instance{{ID: "east"}, {ID: "west", LocalHelm: true}}, true, false, "rewords basic analysis and states the local Helm boundary"},
		{"helmEnvironment on another instance", []profile.Instance{{ID: "east", HelmEnvironment: []profile.HelmEnvVar{{Name: "A"}}}, {ID: "west", LocalHelm: true}}, true, true, "states the environment boundary before the local Helm boundary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := recursiveReport(tc.instances, BackendLimitations{})
			Finalize(context.Background(), &report, profile.Config{Instances: tc.instances}, FinalizeOptions{})
			has := func(text string) bool { return slices.Contains(report.Limitations, text) }
			if has(basicAnalysisLimitation) == tc.local || has(localHelmBasicAnalysisLimitation) != tc.local || has(localHelmLimitation) != tc.local || has(helmEnvironmentLimitation) != tc.env {
				t.Fatalf("Finalize must %s: %q", tc.description, report.Limitations)
			}
			if tc.local && report.Limitations[len(report.Limitations)-1] != localHelmLimitation {
				t.Fatalf("local Helm boundary must be the last limitation: %q", report.Limitations)
			}
			if !tc.local && !strings.Contains(basicAnalysisLimitation, "local Helm") {
				t.Fatalf("basic analysis must point to local Helm for skipped children: %q", basicAnalysisLimitation)
			}
			if !has(secretPayloadLimitation) {
				t.Fatal("local Helm must not reword the Secret payload limitation")
			}
		})
	}
}

// A caller's backend text rewords only its limitation in place; an empty
// field keeps the wording for the profile's backends.
func TestBackendLimitationsRewordOnlyBackendTexts(t *testing.T) {
	report := recursiveReport(nil, BackendLimitations{SecretPayload: "synthetic capabilities"})
	want := recursiveLimitations(BackendLimitations{})
	want[slices.Index(want, secretPayloadLimitation)] = "synthetic capabilities"
	if !slices.Equal(report.Limitations, want) {
		t.Fatalf("only the Secret payload text may change in place; an empty field keeps the default:\n%q", report.Limitations)
	}
	local := []profile.Instance{{ID: "west", LocalHelm: true}}
	if report := recursiveReport(local, BackendLimitations{BasicAnalysis: "synthetic analysis"}); !slices.Contains(report.Limitations, "synthetic analysis") || slices.Contains(report.Limitations, localHelmBasicAnalysisLimitation) {
		t.Fatalf("a caller's basic analysis text must win over the local Helm wording: %q", report.Limitations)
	}
}

type fixedVersion string

func (v fixedVersion) BackendVersion(context.Context) (string, error) { return string(v), nil }

func TestFinalizeAppliesSelectionThenDoctorRenderEvidence(t *testing.T) {
	report := domain.NewReport()
	report.Limitations = []string{"run limitation"}
	report.RenderBackends = []domain.BackendInfo{{Instance: "east", Kind: "argocd-application-api"}}
	selection := []domain.ApplicationSelection{{}}
	Finalize(context.Background(), &report, profile.Config{}, FinalizeOptions{Selections: selection, DoctorRender: true, VersionReaders: map[string]domain.BackendVersionReader{"east": fixedVersion("v3.5.1")}})
	if len(report.Selections) != 1 {
		t.Fatal("selections were not recorded")
	}
	if len(report.Limitations) < 3 || report.Limitations[0] != doctorRenderLimitation || report.Limitations[1] != GeneratedSelectionBoundary || report.Limitations[2] != "run limitation" {
		t.Fatalf("doctor --render boundary must precede the selection boundary and run limitations: %q", report.Limitations)
	}
	if report.RenderBackends[0].Version != "v3.5.1" {
		t.Fatal("doctor --render did not read backend versions through the supplied readers")
	}

	plain := domain.NewReport()
	plain.RenderBackends = []domain.BackendInfo{{Instance: "east", Kind: "argocd-application-api"}}
	Finalize(context.Background(), &plain, profile.Config{}, FinalizeOptions{VersionReaders: map[string]domain.BackendVersionReader{"east": fixedVersion("v3.5.1")}})
	if slices.Contains(plain.Limitations, doctorRenderLimitation) || plain.RenderBackends[0].Version != "" {
		t.Fatal("version evidence and the doctor boundary belong only to doctor --render")
	}
}

// preexistingReport has one complete Application and one whose failure
// predates the PR, so the run is a partial failure.
func preexistingReport() domain.Report {
	broken := domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "broken"}
	issue := domain.Issue{Code: domain.CodeLocalRenderFailed, Stage: "render", Application: &broken}
	report := domain.NewReport()
	report.Revisions = domain.RevisionPair{Base: "base-sha", Head: "head-sha"}
	report.Status = domain.StatusPartialFailure
	report.Applications = []domain.ApplicationResult{
		{ID: domain.ApplicationID{Instance: "east", Namespace: "argocd", Name: "api"}, Execution: domain.ExecutionComplete},
		{ID: broken, Execution: domain.ExecutionFailed, FailureOrigin: domain.FailurePreexisting, Issues: []domain.Issue{issue}},
	}
	report.Issues = []domain.Issue{issue}
	return report
}

func TestFinalizeAppliesDiffStepsOnlyForDiff(t *testing.T) {
	rules := []domain.PolicyRule{{ID: "keep-deployments", Rule: policy.DenyResourceRemoval, Match: domain.PolicySelector{Group: "apps", Kind: "Deployment"}}}
	optIn := profile.Config{Failures: profile.Failures{ExcludePreexistingFromExit: true}, Policies: rules}

	report := preexistingReport()
	Finalize(context.Background(), &report, optIn, FinalizeOptions{Diff: true})
	if report.PreexistingFailuresExcluded != 1 || report.Status != domain.StatusSuccess {
		t.Fatalf("opt-in exclusion did not run: excluded=%d status=%s", report.PreexistingFailuresExcluded, report.Status)
	}
	if report.Policy == nil || !slices.Contains(report.Policy.Limitations, "Applications already failing before this PR are not evaluated (failures.excludePreexistingFromExit); removals in them are unknown on both revisions.") {
		t.Fatal("policies must be evaluated after the preexisting exclusion")
	}

	withoutOptIn := preexistingReport()
	Finalize(context.Background(), &withoutOptIn, profile.Config{Policies: rules}, FinalizeOptions{Diff: true})
	if withoutOptIn.PreexistingFailuresExcluded != 0 || withoutOptIn.Status != domain.StatusPartialFailure || withoutOptIn.Policy == nil {
		t.Fatal("exclusion must stay opt-in while policies still apply to diff")
	}

	doctor := preexistingReport()
	Finalize(context.Background(), &doctor, optIn, FinalizeOptions{})
	if doctor.PreexistingFailuresExcluded != 0 || doctor.Policy != nil || doctor.Status != domain.StatusPartialFailure {
		t.Fatal("exclusion and policies are diff-only")
	}
}

func TestApplyBackendSetupIssuesInvalidConfigurationOnlyWithoutOtherResults(t *testing.T) {
	setup := domain.Issue{Code: domain.CodeConfigurationInvalid, Stage: "backend"}
	other := domain.Issue{Code: domain.CodeLocalRenderFailed, Stage: "render"}
	app := func(execution domain.Execution, issues ...domain.Issue) domain.ApplicationResult {
		return domain.ApplicationResult{ID: domain.ApplicationID{Instance: "west", Namespace: "argocd", Name: "api"}, Execution: execution, Issues: issues}
	}
	for _, tc := range []struct {
		name   string
		apps   []domain.ApplicationResult
		issues []domain.Issue
		want   domain.Status
	}{
		{"only setup failures", nil, []domain.Issue{setup}, domain.StatusInvalidConfiguration},
		{"failed Application with configuration issue", []domain.ApplicationResult{app(domain.ExecutionFailed, setup)}, []domain.Issue{setup}, domain.StatusInvalidConfiguration},
		{"independent result survives", []domain.ApplicationResult{app(domain.ExecutionComplete)}, []domain.Issue{setup}, domain.StatusPartialFailure},
		{"mixed failure causes", []domain.ApplicationResult{app(domain.ExecutionFailed, other)}, []domain.Issue{setup}, domain.StatusFailed},
		{"no setup issue", []domain.ApplicationResult{app(domain.ExecutionComplete)}, nil, domain.StatusSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := domain.NewReport()
			report.Applications = tc.apps
			ApplyBackendSetupIssues(&report, tc.issues)
			if report.Status != tc.want || len(report.Issues) != len(tc.issues) {
				t.Fatalf("status=%s issues=%d want %s with every setup issue recorded", report.Status, len(report.Issues), tc.want)
			}
		})
	}
}

func TestRevisionFailureReportMapsCauseToStatus(t *testing.T) {
	c := profile.Config{Git: profile.Git{Repository: "example/platform"}}
	fail := func(code string) error { return domain.Fail(code, "revision", "reason", "action", nil) }
	for _, tc := range []struct {
		name      string
		err       error
		cancelled bool
		want      domain.Status
		code      string
	}{
		{"invalid reference", fail(domain.CodeRevisionInvalid), false, domain.StatusInvalidConfiguration, domain.CodeRevisionInvalid},
		{"invalid configuration", fail(domain.CodeConfigurationInvalid), false, domain.StatusInvalidConfiguration, domain.CodeConfigurationInvalid},
		{"denied", fail(domain.CodeAccessDenied), false, domain.StatusDenied, domain.CodeAccessDenied},
		{"provider failure", fail(domain.CodeNotFound), false, domain.StatusFailed, domain.CodeNotFound},
		{"untyped error", errors.New("boom"), false, domain.StatusFailed, domain.CodeInternalError},
		{"cancellation wins", fail(domain.CodeAccessDenied), true, domain.StatusCancelled, domain.CodeAccessDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			report := RevisionFailureReport(ctx, c, false, tc.err)
			if report.Status != tc.want || len(report.Issues) != 1 || report.Issues[0].Code != tc.code || report.Issues[0].Repository != "example/platform" || report.Revisions.Repository != "example/platform" {
				t.Fatalf("status=%s issues=%+v", report.Status, report.Issues)
			}
			if len(report.Applications) != 0 || report.ChangesFound {
				t.Fatal("a revision failure must not report compared targets")
			}
		})
	}
	for _, tc := range []struct {
		name      string
		config    profile.Config
		recursive bool
		want      string
	}{
		{"explicit", c, false, "explicit-applications"},
		{"recursive", c, true, "recursive-target-graph"},
		{"repository discovery wins", profile.Config{Discovery: &profile.Discovery{}}, true, "repository-discovered-target-graph"},
	} {
		t.Run("coverage "+tc.name, func(t *testing.T) {
			if got := RevisionFailureReport(context.Background(), tc.config, tc.recursive, errors.New("boom")).Coverage; got != tc.want {
				t.Fatalf("coverage=%q want %q", got, tc.want)
			}
		})
	}
}

// The preexisting opt-in can only exclude a failure whose unchanged inputs a
// PR comparison with changed-path selection proved; elsewhere the report says
// first that the setting had no effect.
func TestPreexistingOptInSaysWhenItCannotApply(t *testing.T) {
	changed := &profile.Discovery{Mode: "repository", Selection: profile.SelectionChangedPaths}
	all := &profile.Discovery{Mode: "repository", Selection: "all"}
	for _, tc := range []struct {
		name      string
		mode      string
		discovery *profile.Discovery
		optIn     bool
		note      bool
	}{
		{"PR with changed-path selection", domain.RevisionModePR, changed, true, false},
		{"PR selecting all candidates", domain.RevisionModePR, all, true, true},
		{"explicit base and head", "explicit-base-head", changed, true, true},
		{"explicit Applications", domain.RevisionModePR, nil, true, true},
		{"opt-in off", domain.RevisionModePR, all, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := domain.NewReport()
			report.Revisions.Mode = tc.mode
			c := profile.Config{Discovery: tc.discovery, Failures: profile.Failures{ExcludePreexistingFromExit: tc.optIn}}
			Finalize(context.Background(), &report, c, FinalizeOptions{Diff: true})
			got := len(report.Limitations) > 0 && report.Limitations[0] == preexistingUnprovable
			if got != tc.note {
				t.Fatalf("note shown=%t, want %t: %v", got, tc.note, report.Limitations)
			}
		})
	}
}
