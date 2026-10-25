package domain

type RevisionPair struct {
	Repository string `json:"repository"`
	Base       string `json:"base"`
	Head       string `json:"head"`
	Mode       string `json:"mode"`
}

// RevisionPair modes: a PR's merge base and head, or two explicit references.
const (
	RevisionModePR       = "pr-merge-base-head"
	RevisionModeExplicit = "explicit-base-head"
)

// PreexistingExcludedNotice names failures left out of the exit code by the
// failures.excludePreexistingFromExit opt-in; every report format states it
// next to the status.
const PreexistingExcludedNotice = "%d Application(s) were already failing before this PR; they are excluded from the exit code by failures.excludePreexistingFromExit. Their resource changes are unknown."

type Report struct {
	Discovery        []DiscoveryResult      `json:"discovery,omitempty"`
	Selections       []ApplicationSelection `json:"selections,omitempty"`
	RenderBackends   []BackendInfo          `json:"renderBackends,omitempty"`
	Graph            *GraphSummary          `json:"graph,omitempty"`
	Policy           *PolicySummary         `json:"policy,omitempty"`
	SchemaVersion    string                 `json:"schemaVersion"`
	Revisions        RevisionPair           `json:"revisions"`
	Status           Status                 `json:"status"`
	ChangesFound     bool                   `json:"changesFound"`
	Redaction        string                 `json:"redaction"`
	Coverage         string                 `json:"coverage"`
	Applications     []ApplicationResult    `json:"applications"`
	Issues           []Issue                `json:"issues"`
	PolicyViolations []string               `json:"policyViolations"`
	Limitations      []string               `json:"limitations"`
	// PreexistingFailuresExcluded counts Applications already failing before
	// the PR that an operator opt-in left out of the exit code.
	PreexistingFailuresExcluded int `json:"preexistingFailuresExcluded,omitempty"`
	// Generator names the build and profile shape of a diff or doctor run
	//; demo reports omit it.
	Generator *Generator `json:"generator,omitempty"`
}

// CoverageConnectionPreflight is the coverage of doctor without --render: it
// reads access only and compares no targets, so its Applications are not
// shown as unchanged in human-readable text.
const CoverageConnectionPreflight = "connection-preflight"

func NewReport() Report {
	return Report{SchemaVersion: "v1alpha1", Status: StatusSuccess, Redaction: "enabled", Coverage: "explicit-applications", Applications: []ApplicationResult{}, Issues: []Issue{}, PolicyViolations: []string{}, Limitations: []string{}}
}

// SetAggregateStatus sets the report status from per-Application outcomes:
// cancelled wins; failures with useful independent results are a partial
// failure; failures that are all access denials are denied; other failures
// are failed. Without failures the current status is kept.
func (r *Report) SetAggregateStatus(cancelled bool, failed, denied int, useful bool) {
	switch {
	case cancelled:
		r.Status = StatusCancelled
	case failed > 0 && useful:
		r.Status = StatusPartialFailure
	case failed > 0 && failed == denied:
		r.Status = StatusDenied
	case failed > 0:
		r.Status = StatusFailed
	}
}

func (r Report) ExitCode() int {
	switch r.Status {
	case StatusInvalidConfiguration:
		return ExitInvalidConfiguration
	case StatusDenied:
		return ExitDenied
	case StatusFailed:
		return ExitFailed
	case StatusCancelled:
		return ExitCancelled
	case StatusPartialFailure:
		return ExitPartialFailure
	case StatusPolicyViolation:
		return ExitPolicyViolation
	}
	if r.ChangesFound {
		return ExitChangesFound
	}
	return ExitSuccess
}

type ApplicationResult struct {
	BaseRepeatedResources []ResourceID    `json:"baseRepeatedResources,omitempty"`
	HeadRepeatedResources []ResourceID    `json:"headRepeatedResources,omitempty"`
	BaseSources           []SourceSummary `json:"baseSources,omitempty"`
	HeadSources           []SourceSummary `json:"headSources,omitempty"`
	ID                    ApplicationID   `json:"id"`
	Root                  ApplicationID   `json:"root"`
	GraphPath             []ApplicationID `json:"graphPath"`
	Project               string          `json:"project"`
	Cluster               string          `json:"cluster"`
	Namespace             string          `json:"namespace"`
	State                 State           `json:"state"`
	Execution             Execution       `json:"execution"`
	Inputs                InputSummary    `json:"inputs"`
	Resources             []ResourceDiff  `json:"resources"`
	Issues                []Issue         `json:"issues"`
	SkipReason            string          `json:"skipReason,omitempty"`
	BaseTarget            *TargetSummary  `json:"baseTarget,omitempty"`
	HeadTarget            *TargetSummary  `json:"headTarget,omitempty"`
	SpecChanges           []Change        `json:"specChanges,omitempty"`
	InputChanges          []InputChange   `json:"inputChanges,omitempty"`
	// FailureOrigin is set when the Application has base and head target
	// summaries and at least one of them failed; it compares the two outcomes
	//. The values are the FailureOrigin constants.
	FailureOrigin FailureOrigin  `json:"failureOrigin,omitempty"`
	Pruning       *PruningImpact `json:"pruning,omitempty"`
	// PlaceholderInput is set when the Application, or an ancestor, was
	// rendered from a desired spec with fields that differed between two
	// renders of identical inputs.
	PlaceholderInput *PlaceholderInput `json:"placeholderInput,omitempty"`
}

// PlaceholderInput names the Application whose rendered desired spec held
// RenderUnstable at Paths. Descendants rendered from it carry the same value.
type PlaceholderInput struct {
	Application ApplicationID `json:"application"`
	Paths       []string      `json:"paths"`
}

type InputSummary struct {
	Path           string   `json:"path"`
	ValueFiles     []string `json:"valueFiles"`
	Parameters     []string `json:"parameters"`
	FileParameters []string `json:"fileParameters"`
	ReleaseName    string   `json:"releaseName,omitempty"`
	InlineValues   bool     `json:"inlineValues"`
	ValuesObject   bool     `json:"valuesObject"`
	Origin         string   `json:"origin"`
	// IgnoreDifferences counts spec.ignoreDifferences rules. They are not
	// applied to the target comparison (see Application.IgnoreRulesAffectTargets).
	IgnoreDifferences int `json:"ignoreDifferences,omitempty"`
}

type TargetSummary struct {
	SkipReason        string          `json:"skipReason,omitempty"`
	RepeatedResources []ResourceID    `json:"repeatedResources,omitempty"`
	Sources           []SourceSummary `json:"sources,omitempty"`
	Repository        string          `json:"repository"`
	Revision          string          `json:"revision"`
	RevisionRule      string          `json:"revisionRule"`
	// RequestedRevision is the branch, tag or HEAD that Revision was
	// resolved from at run time (revisionRule resolved-source-reference).
	RequestedRevision string          `json:"requestedRevision,omitempty"`
	Execution         Execution       `json:"execution"`
	Project           string          `json:"project"`
	Cluster           string          `json:"cluster"`
	Namespace         string          `json:"namespace"`
	Inputs            InputSummary    `json:"inputs"`
	Root              ApplicationID   `json:"root"`
	GraphPath         []ApplicationID `json:"graphPath"`
}

type GraphEdge struct {
	Parent     ApplicationID `json:"parent"`
	Child      ApplicationID `json:"child"`
	Repository string        `json:"repository"`
	Revision   string        `json:"revision"`
	Method     string        `json:"method"`
	Routing    string        `json:"routing"`
}

type GraphSummary struct {
	Changes      []GraphEdgeChange `json:"changes"`
	BaseComplete bool              `json:"baseComplete"`
	HeadComplete bool              `json:"headComplete"`
	BaseEdges    []GraphEdge       `json:"baseEdges"`
	HeadEdges    []GraphEdge       `json:"headEdges"`
}

type GraphEdgeChange struct {
	Parent ApplicationID `json:"parent"`
	Child  ApplicationID `json:"child"`
	State  State         `json:"state"`
}

type PruningImpact struct {
	Certainty string       `json:"certainty"`
	Reason    string       `json:"reason"`
	Resources []ResourceID `json:"resources"`
}

type BackendInfo struct {
	HelmVersion string `json:"helmVersion,omitempty"`
	HelmSHA256  string `json:"helmSHA256,omitempty"`
	// HelmEnvironment names operator-configured variables passed to an
	// external Helm executable. Values are never reported.
	HelmEnvironment []string `json:"helmEnvironment,omitempty"`
	Version         string   `json:"version,omitempty"`
	Compatibility   string   `json:"compatibility,omitempty"`
	Instance        string   `json:"instance"`
	Kind            string   `json:"kind"`
	ContextID       string   `json:"contextID"`
	Verification    string   `json:"verification"`
}

// BackendInfo.Kind of the detailed render backends.
const (
	BackendLocalHelm  = "local-helm"
	BackendRepoServer = "argocd-repo-server"
)
