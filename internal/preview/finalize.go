package preview

import (
	"context"
	"errors"

	"diffaro/internal/domain"
	"diffaro/internal/policy"
	"diffaro/internal/profile"
)

// Report texts for local Helm and doctor --render.
const (
	localHelmBasicAnalysisLimitation = domain.LimitationLocalBasicAnalysis
	helmEnvironmentLimitation        = "An external Helm executable received operator-configured environment variables (names under renderBackends). Templates may read services outside Git; base and head render at different times, so such values are not tied to either revision and are not verified."
	localHelmLimitation              = domain.LimitationLocalHelm
	doctorRenderLimitation           = "Doctor rendered only the selected revisions and scope; this does not certify future PRs, unselected children or mutable backend dependencies."
)

// ComparisonEvidence carries what a command collected around a comparison
// that the steps after the run state in the report.
type ComparisonEvidence struct {
	// SelectionNotice states why changed-path selection rendered every
	// candidate; empty when it did not.
	SelectionNotice string
	// Files lists the PR's changed files for inputChanges; nil without a
	// complete PR file list.
	Files *InputFiles
	// FileListFailure states why the PR file list could not be read
	// (FileListLimitation); empty when it was read or not needed.
	FileListFailure string
	// Notes returns the backend notes. It is called after every other step
	// because input change reads can add to them; nil when there are none.
	Notes func() []string
	// WithoutInputs is set by a command that has no pull request by design,
	// such as demo: input changes are neither read nor reported as needing
	// --pr.
	WithoutInputs bool
}

// CompleteComparison applies the report steps that follow every comparison
// in their fixed order: the changed-path selection notice, masking of
// repeated values, input changes, the PR file list failure as
// the first limitation, failure origins, then backend notes. Finalize
// follows it.
func (r Runner) CompleteComparison(ctx context.Context, report *domain.Report, e ComparisonEvidence) {
	r.completeComparison(ctx, report, e, nil)
}

func (r Runner) completeComparison(ctx context.Context, report *domain.Report, e ComparisonEvidence, head *changedPathSelector) {
	if e.SelectionNotice != "" {
		report.Limitations = append(report.Limitations, e.SelectionNotice)
	}
	// Every target is compared now, so a value masked in any of them is
	// masked where it reappears in another resource.
	MaskRepeatedValues(report, r.Redactor.Masked)
	if !e.WithoutInputs {
		r.attachInputChanges(ctx, report, e.Files, head)
	}
	if e.FileListFailure != "" {
		// First, so the compact PR Markdown shows the cause.
		report.Limitations = append([]string{e.FileListFailure}, report.Limitations...)
	}
	AnnotateFailureOrigins(report)
	if e.Notes != nil {
		report.Limitations = append(report.Limitations, e.Notes()...)
	}
}

// FinalizeOptions carries what a command collected around a run that the
// report must still state.
type FinalizeOptions struct {
	// Selections and SelectionIssues come from SelectGenerated and
	// VerifySelections.
	Selections      []domain.ApplicationSelection
	SelectionIssues []domain.Issue
	// SetupIssues name instances whose configured backend failed to start.
	SetupIssues []domain.Issue
	// Diff applies the diff-only steps: the opt-in preexisting failure
	// exclusion and configured policies.
	Diff bool
	// DoctorRender records backend version evidence from VersionReaders and
	// states the doctor --render boundary. A diff records the versions only.
	DoctorRender   bool
	VersionReaders map[string]domain.BackendVersionReader
}

// Finalize applies the report steps that follow a run in their fixed order:
// generated selection, backend setup failures, local Helm boundaries, then
// either the diff-only steps or doctor --render evidence. The backend texts
// follow the profile's instances, never the caller's wording.
func Finalize(ctx context.Context, report *domain.Report, c profile.Config, o FinalizeOptions) {
	ApplySelectionReport(report, o.Selections, o.SelectionIssues)
	ApplyBackendSetupIssues(report, o.SetupIssues)
	nameTimeoutLimit(report, c.Render.Timeout)
	applyLocalHelmLimitations(report, c.Instances)
	if o.Diff {
		if c.Failures.ExcludePreexistingFromExit {
			ExcludePreexistingFailures(report)
			if !preexistingProvable(report, c) {
				report.Limitations = append([]string{preexistingUnprovable}, report.Limitations...)
			}
		}
		policy.Apply(report, c.Policies)
		RecordBackendVersions(ctx, report, o.VersionReaders)
	}
	if o.DoctorRender {
		InspectBackendVersions(ctx, report, o.VersionReaders)
		report.Limitations = append([]string{doctorRenderLimitation}, report.Limitations...)
	}
}

// preexistingUnprovable states why failures.excludePreexistingFromExit
// cannot exclude anything in a run.
const preexistingUnprovable = "failures.excludePreexistingFromExit has no effect in this run: proving that a failing Application's inputs are unchanged needs --pr and discovery.selection: changedPaths, so its failures count toward the exit code."

// preexistingProvable reports whether the run can prove unchanged inputs,
// which the preexisting failure origin requires: a PR comparison with
// changed-path selection.
func preexistingProvable(report *domain.Report, c profile.Config) bool {
	return report.Revisions.Mode == domain.RevisionModePR && c.Discovery != nil && c.Discovery.Selection == profile.SelectionChangedPaths
}

// applyLocalHelmLimitations states local Helm coverage boundaries when any
// instance uses local Helm. A graph run has already worded its basic
// analysis limitation for local Helm (backendLimitations).
func applyLocalHelmLimitations(report *domain.Report, instances []profile.Instance) {
	local, environment := false, false
	for _, instance := range instances {
		local = local || instance.LocalHelm
		environment = environment || len(instance.HelmEnvironment) > 0
	}
	if !local {
		return
	}
	if environment {
		report.Limitations = append(report.Limitations, helmEnvironmentLimitation)
	}
	report.Limitations = append(report.Limitations, localHelmLimitation)
}

// ApplyBackendSetupIssues records instances whose configured backend failed
// to start. When no independent work succeeded and every observed failure is
// invalid backend configuration, the status is invalid_configuration (exit 4);
// mixed failures keep their aggregate status and partial results exit 3.
func ApplyBackendSetupIssues(report *domain.Report, issues []domain.Issue) {
	if len(issues) == 0 {
		return
	}
	ApplyRunIssues(report, issues)
	if report.Status != domain.StatusFailed {
		return
	}
	for _, issue := range report.Issues {
		if issue.Code != domain.CodeConfigurationInvalid {
			return
		}
	}
	for _, app := range report.Applications {
		for _, issue := range app.Issues {
			if issue.Code != domain.CodeConfigurationInvalid {
				return
			}
		}
	}
	report.Status = domain.StatusInvalidConfiguration
}

// RevisionFailureReport reports a base/head resolution failure without
// rendering anything. Its coverage names the preview mode that produced no
// result: repository discovery, recursive or explicit Applications.
func RevisionFailureReport(ctx context.Context, c profile.Config, recursive bool, err error) domain.Report {
	report := domain.NewReport()
	report.Revisions.Repository = c.Git.Repository
	report.Coverage = requestedCoverage(c, recursive)
	report.Status = domain.StatusFailed
	issue := domain.UnclassifiedIssue(err, "revision", "revision resolution failed")
	var failure *domain.Failure
	if errors.As(err, &failure) {
		issue = failure.Issue
	}
	issue.Repository = c.Git.Repository
	report.Issues = append(report.Issues, issue)
	if issue.Code == domain.CodeConfigurationInvalid || issue.Code == domain.CodeRevisionInvalid {
		report.Status = domain.StatusInvalidConfiguration
	}
	if issue.Code == domain.CodeAccessDenied {
		report.Status = domain.StatusDenied
	}
	if ctx.Err() != nil {
		report.Status = domain.StatusCancelled
	}
	return report
}

func requestedCoverage(c profile.Config, recursive bool) string {
	switch {
	case c.Discovery != nil:
		return "repository-discovered-target-graph"
	case recursive:
		return "recursive-target-graph"
	}
	return "explicit-applications"
}
