package helmerror

import (
	"strings"

	"diffaro/internal/domain"
)

// RenderFailureCause classifies an Argo CD render failure by the phrases
// Argo CD v3 puts in its GenerateManifest error, with the same reason codes
// and template location as local Helm.
// backend names who rendered, such as "repo-server" or "Argo CD". The texts
// it returns never quote the message. It reports false for a message without
// a known render failure, such as a transport or gateway error.
func RenderFailureCause(raw, backend string) (code, reason, action string, ok bool) {
	const withheld = "; the raw message is withheld to protect values"
	text := strings.ToLower(raw)
	switch {
	case strings.Contains(text, "app path does not exist"):
		return domain.ReasonChartPathMissing, "chart path does not exist at the requested revision", "check the Application source path and targetRevision; Argo CD reports \"app path does not exist\"", true
	case strings.Contains(text, "error reading helm chart from"):
		return domain.ReasonChartMetadataMissing, "chart path has no readable Chart.yaml at the requested revision", "check that the source path is a Helm chart at this revision", true
	case strings.Contains(text, "unable to resolve") && (strings.Contains(text, "to a commit sha") || strings.Contains(text, "git revision")):
		return domain.ReasonRevisionMissing, backend + " could not resolve the source revision", "check that the revision exists in the repository this instance reads", true
	case !strings.Contains(text, "helm template") && !strings.Contains(text, "failed to generate manifest"):
		return "", "", "", false
	case strings.Contains(text, "error: open ") && strings.Contains(text, "no such file or directory"):
		return domain.ReasonValueFileMissing, "a value file or file parameter does not exist at the requested revision", "check the Application valueFiles and fileParameters at this revision; Argo CD fails a missing value file unless ignoreMissingValueFiles is set", true
	}
	if detail, found := Describe(raw); found {
		return domain.ReasonTemplateFailed, detail + withheld, "inspect that template line and its Helm inputs for this Application and revision; " + ReproduceHint, true
	}
	if strings.Contains(text, "yaml parse error") {
		return domain.ReasonRenderedOutputInvalid, "Helm rendered output that is not valid YAML" + withheld, "inspect the chart's templates and Helm inputs for this Application and revision", true
	}
	return domain.ReasonTemplateFailed, "Helm template rendering failed in " + backend + withheld, "inspect the chart's templates, values files and source revision in the trusted instance's backend logs", true
}
