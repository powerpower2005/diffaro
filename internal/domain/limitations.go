package domain

// Standing limitations are the scope boundaries a report states whatever
// the run found: what a comparison of their kind never covers. JSON and
// Markdown list them in full; the terminal report counts them on one line
// so the limitations of this run stay visible.
const (
	LimitationExplicitScope         = "Scope is explicitly configured Applications; this is not complete graph discovery."
	LimitationExplicitSecretPayload = "Argo CD masks Secret payloads; existing Secret payload equality is not observable."
	LimitationExplicitBackend       = "Backend capabilities, dependencies and renderer environment must remain stable; system ignore rules are not imported."

	LimitationRootSnapshot       = "Roots use one stored-spec snapshot; selected-repository nodes use the requested base/head SHA."
	LimitationOtherRepositories  = "Other repositories require an immutable 40-character targetRevision; their selection is reported per node."
	LimitationBasicAnalysis      = "Basic analysis uses the Application API; new or reconfigured child resource rendering and descendant discovery are skipped. Select local Helm or configure repo-server for optional detailed analysis."
	LimitationLocalBasicAnalysis = "Instances using the basic Application API skip new or reconfigured child resource rendering and descendant discovery; explicitly configured local Helm instances render supported desired child specs."
	LimitationMissingNodes       = "Missing nodes are added/removed only when opposite discovery is complete for their instance; configured routes from incomplete subtrees prevent absence claims in reachable instances."
	LimitationRemovedTargets     = "Removed targets are pruning candidates, not proof of live deletion; parent prune policy, child finalizers and cascade behavior must be checked."
	LimitationSecretPayload      = "Argo CD masks Secret payloads; existing payload equality is not observable. Backend capabilities must stay stable; system ignore rules are not imported."

	LimitationDiscoveryBoundary  = "Selection covers readable Applications using the PR repository and their rendered descendants; hidden apps and indirect chart dependencies in other repositories are not automatically identified."
	LimitationGeneratedSelection = "Only currently readable generated Applications are selected; RBAC-hidden apps and base/head ApplicationSet membership or template changes are not evaluated."
	LimitationLocalHelm          = "Local Helm uses the bundled engine or an explicitly selected trusted executable with authenticated Argo CD API settings and cached capabilities. Custom server tooling/environment and unavailable context are not certified; skipped branches remain incomplete. No repo-server, Kubernetes credentials or repository scripts are used."
	LimitationVersionMatrix      = "Version matching the tested matrix does not verify this instance's credentials, global settings, mutable dependencies or render context; verification remains live-unverified."
	LimitationVersionLocalHelm   = "Argo CD server version does not certify the bundled Helm engine or custom instance environment; local Helm verification remains local-context-unverified."
)

var standingLimitations = map[string]bool{
	LimitationExplicitScope: true, LimitationExplicitSecretPayload: true, LimitationExplicitBackend: true,
	LimitationRootSnapshot: true, LimitationOtherRepositories: true, LimitationBasicAnalysis: true, LimitationLocalBasicAnalysis: true,
	LimitationMissingNodes: true, LimitationRemovedTargets: true, LimitationSecretPayload: true,
	LimitationDiscoveryBoundary: true, LimitationGeneratedSelection: true, LimitationLocalHelm: true,
	LimitationVersionMatrix: true, LimitationVersionLocalHelm: true,
}

// StandingLimitation reports whether text is a standing limitation.
func StandingLimitation(text string) bool {
	return standingLimitations[text]
}
