// Package argopolicy evaluates Argo CD AppProject rules: repository URL
// normalization, glob patterns, sourceRepos and destination permissions
// (Authorize). It is shared by the local Helm and repo-server backends and does
// not perform any provider calls.
//
// It is unrelated to package policy, which evaluates Diffaro report policies.
package argopolicy

import "diffaro/internal/domain"

// Authorize preserves AppProject policy for a proposed Application without
// creating it. Server/name identity must come from authenticated discovery.
func Authorize(project map[string]any, repository string, destination map[string]any, applicationNamespace, controllerNamespace string) error {
	if keys, _ := project["signatureKeys"].([]any); len(keys) > 0 || project["sourceIntegrity"] != nil || project["permitOnlyProjectScopedClusters"] == true {
		return domain.Fail(domain.CodeUnsupportedProjectPolicy, "permission", "this backend cannot verify the project integrity or cluster-scoping policy", "use a backend supporting the configured project policy", nil)
	}
	allowed, err := SourceAllowed(project["sourceRepos"], repository)
	if err != nil {
		return err
	}
	if !allowed {
		return denied("project does not permit this source repository")
	}
	allowed, err = DestinationAllowed(project["destinations"], destination)
	if err != nil {
		return err
	}
	if !allowed {
		return denied("project does not permit this destination")
	}
	if applicationNamespace != controllerNamespace {
		allowed, err = SourceNamespaceAllowed(project["sourceNamespaces"], applicationNamespace)
		if err != nil {
			return err
		}
		if !allowed {
			return denied("project does not permit the Application namespace")
		}
	}
	return nil
}

func denied(reason string) error {
	return domain.Fail(domain.CodeAccessDenied, "permission", reason, "review the desired repository, destination and Application namespace in this AppProject", nil)
}
