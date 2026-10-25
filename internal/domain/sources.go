package domain

import (
	"context"
	"reflect"
)

// SourceRevision records one entry of an ordered Application source plan.
// Source carries original renderer settings plus the selected targetRevision.
type SourceRevision struct {
	Position   int    `json:"position"`
	Repository string `json:"repository"`
	Ref        string `json:"ref,omitempty"`
	Revision   string `json:"revision"`
	Rule       string `json:"rule"`
	// Requested is the branch, tag or HEAD that Revision was resolved from
	// at run time (rule resolved-source-reference), otherwise empty.
	Requested string         `json:"requestedRevision,omitempty"`
	Source    map[string]any `json:"-"`
}

// Matches reports whether s is the plan entry for source, the Application
// source at index: same position, repository and ref, an immutable commit SHA
// as targetRevision, and every other input unchanged. Backends reject a plan
// with any non-matching entry before rendering.
func (s SourceRevision) Matches(source any, index int) bool {
	original, ok := source.(map[string]any)
	if !ok || s.Position != index+1 || !IsCommitSHA(s.Revision) || s.Repository != String(original, "repoURL") || s.Ref != String(original, "ref") {
		return false
	}
	left, right := CloneSpec(original), CloneSpec(s.Source)
	delete(left, "targetRevision")
	delete(right, "targetRevision")
	return reflect.DeepEqual(left, right) && String(s.Source, "targetRevision") == s.Revision
}

type SourceSummary struct {
	Position   int    `json:"position"`
	Repository string `json:"repository"`
	Ref        string `json:"ref,omitempty"`
	Revision   string `json:"revision"`
	Rule       string `json:"rule"`
	// RequestedRevision is the reference Revision was resolved from at run
	// time (rule resolved-source-reference), otherwise empty.
	RequestedRevision string       `json:"requestedRevision,omitempty"`
	Inputs            InputSummary `json:"inputs"`
}

type MultiSourceRenderer interface {
	RenderSources(context.Context, Application, []SourceRevision) ([]string, error)
	RenderDesiredSources(context.Context, Application, []SourceRevision) ([]string, error)
}

type SourceRevisionResolver interface {
	ResolveSourceRevision(context.Context, Application, string, string) (string, error)
}

// HelmSourceFields are the fields of an Argo CD Application source's helm
// block (ApplicationSourceHelm). Callers must not modify it.
var HelmSourceFields = []string{"valueFiles", "values", "valuesObject", "parameters", "fileParameters", "releaseName", "namespace", "version", "kubeVersion", "apiVersions", "ignoreMissingValueFiles", "skipCrds", "skipSchemaValidation", "skipTests", "passCredentials"}

// nonHelmSourceKeys select Argo CD source types other than Git Helm.
var nonHelmSourceKeys = []string{"plugin", "kustomize", "directory", "chart"}

// UsesNonHelmSource reports whether an Application source selects a CMP,
// Kustomize, directory or Helm chart repository source, which no backend
// renders.
func UsesNonHelmSource(source map[string]any) bool {
	for _, key := range nonHelmSourceKeys {
		if value := source[key]; value != nil && value != "" {
			return true
		}
	}
	return false
}

// IgnoreRuleCount returns the number of spec.ignoreDifferences rules.
func (a Application) IgnoreRuleCount() int {
	rules, _ := a.Spec["ignoreDifferences"].([]any)
	return len(rules)
}

// IgnoreRulesAffectTargets reports whether ignore rules change what Argo CD
// applies. Without the RespectIgnoreDifferences=true sync option they only
// affect live drift status, so the rendered targets remain comparable; with
// it, ignored fields keep their live values on sync and a target diff of
// those fields would not describe the applied result.
func (a Application) IgnoreRulesAffectTargets() bool {
	if a.IgnoreRuleCount() == 0 {
		return false
	}
	policy, _ := a.Spec["syncPolicy"].(map[string]any)
	options, _ := policy["syncOptions"].([]any)
	for _, option := range options {
		if option == "RespectIgnoreDifferences=true" {
			return true
		}
	}
	return false
}

// SupportedSource reports whether an Application uses a supported explicit
// Git Helm source (single or multiple). Preview checks it before choosing a
// backend, and backends repeat it so that no caller renders an unsupported
// source.
func SupportedSource(a Application) error {
	if sources, ok := a.Spec["sources"].([]any); ok && len(sources) > 0 {
		return supportedSources(a, sources)
	}
	s := a.Source()
	if _, present := s["helm"].(map[string]any); !present {
		return Fail(CodeUnsupportedSource, "selection", "the Application API response has no explicit spec.source.helm configuration", "use an explicitly configured Helm Application; Argo CD omits empty helm: {} objects, so the administrator must provide a meaningful Helm option such as the intended releaseName", nil)
	}
	if a.Spec["sourceHydrator"] != nil {
		return Fail(CodeUnsupportedSource, "selection", "source hydration requires a separately verified adapter", "select an Application without sourceHydrator", nil)
	}
	if UsesNonHelmSource(s) {
		return Fail(CodeUnsupportedSource, "selection", "source type requires a separately verified render adapter", "use a Git-hosted Helm chart without CMP for the initial backend", nil)
	}
	if a.IgnoreRulesAffectTargets() {
		return Fail(CodeUnsupportedIgnoreRules, "selection", "ignore rules with RespectIgnoreDifferences=true change what Argo CD applies and are not supported", "compare this Application without RespectIgnoreDifferences or review its ignored fields manually", nil)
	}
	return nil
}

func supportedSources(app Application, sources []any) error {
	if len(sources) > 32 {
		return Fail(CodeSourceInvalid, "selection", "source count exceeds 32", "reduce Application source count", nil)
	}
	for _, raw := range sources {
		source, ok := raw.(map[string]any)
		if !ok || String(source, "repoURL") == "" {
			return Fail(CodeSourceInvalid, "selection", "source is missing its repository", "provide valid Application sources", nil)
		}
		copy := CloneSpec(app.Spec)
		delete(copy, "sources")
		copy["source"] = CloneSpec(source)
		// A ref without a path contributes external values but no resources.
		if String(source, "ref") != "" && String(source, "path") == "" {
			for _, key := range []string{"helm", "chart", "plugin", "kustomize", "directory"} {
				if source[key] != nil {
					return Fail(CodeUnsupportedSource, "selection", "values-only source has renderer settings", "provide a values ref or an explicit supported rendering source", nil)
				}
			}
			copy["source"].(map[string]any)["helm"] = map[string]any{}
		}
		if err := SupportedSource(Application{ID: app.ID, Spec: copy}); err != nil {
			return err
		}
	}
	return nil
}
