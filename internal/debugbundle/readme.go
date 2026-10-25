package debugbundle

import "strings"

// readme is the bundle's README.md: what it holds and what it leaves out,
// so the operator can check it before attaching it.
func readme(level string) string {
	var b strings.Builder
	b.WriteString("# Diffaro debug bundle (" + level + ")\n\n")
	b.WriteString("Created by `diffaro diff --debug-bundle`. Read this file and `bundle.json` before you share the bundle.\n\n")
	b.WriteString("## Contents\n\n")
	b.WriteString("- `bundle.json`: the Diffaro build and profile shape, the option names given (not their values), status and exit code, counts, render backend kinds and Argo CD versions, selection decisions, Application states, issue codes with their diagnostics (source line, cause types, HTTP status, route template, panic stack) and trace steps with their timing.\n")
	if level == LevelPublic {
		b.WriteString("\n## Left out\n\n")
		b.WriteString("- Hosts, organizations, repositories, revisions, Argo CD instances, namespaces, Application names, projects, credential variable names and trace subjects are replaced by pseudonyms such as `app-1f3a9c0b2e`. The pseudonyms are keyed by a random key drawn for this bundle only. The file `<bundle>.names.json` written next to the bundle maps them back to your names: keep it private and do not attach it.\n")
		b.WriteString("- Free text is left out: failure reasons, actions, skip reasons and limitations. Values, manifests, diffs, value files, trace digests, error messages and credentials are not collected.\n")
		b.WriteString("\nThis level is meant for public issues.\n")
		return b.String()
	}
	b.WriteString("- `report.json`: the full JSON report with the report's redaction applied.\n")
	b.WriteString("- `trace.json`: the phase trace with the report's redaction applied.\n")
	b.WriteString("\n## Left out\n\n")
	b.WriteString("- Credentials and raw error messages are never collected. Names, revisions and redacted resource changes are kept.\n")
	b.WriteString("\nThis level is meant for your team or a private channel only. For a public issue, use `--debug-bundle-level public`.\n")
	return b.String()
}
