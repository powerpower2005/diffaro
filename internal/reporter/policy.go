package reporter

import (
	"fmt"
	"strings"

	"diffaro/internal/domain"
)

func writePolicy(b *strings.Builder, report domain.Report, compact bool) {
	if report.Policy == nil {
		return
	}
	text := esc
	limit := len(report.Policy.Violations)
	if compact {
		text = func(s string) string { return prText(s, 200) }
		limit = min(limit, 12)
	}
	fmt.Fprintf(b, "## Policies\n\nEvaluation: `%s` · scope: `%s` · %d violation(s)\n\n", text(report.Policy.Evaluation), text(report.Policy.Scope), len(report.Policy.Violations))
	for _, rule := range report.Policy.Rules {
		fmt.Fprintf(b, "- Configured `%s` (`%s`): group `%s`, kind `%s`, instance `%s`, project `%s`, namespace `%s`, name `%s`.\n", text(rule.ID), text(rule.Rule), text(rule.Match.Group), text(rule.Match.Kind), text(rule.Match.Instance), text(rule.Match.Project), text(rule.Match.Namespace), text(rule.Match.Name))
	}
	b.WriteString("\n")
	for _, violation := range report.Policy.Violations[:limit] {
		fmt.Fprintf(b, "- **%s** (`%s`): Application `%s`, resource `%s`. %s\n", text(violation.PolicyID), text(violation.RuleID), text(violation.Application.String()), text(violation.Resource.String()), text(violation.Reason))
		fmt.Fprintf(b, "  - Evidence `%s`: `%t` → `%t`.\n", text(violation.Evidence.Path), violation.Evidence.Before, violation.Evidence.After)
		for _, side := range []struct {
			name  string
			value domain.PolicyRevision
		}{{"Base", violation.Base}, {"Head", violation.Head}} {
			if side.value.Absent {
				fmt.Fprintf(b, "  - %s Application target: absent.\n", side.name)
				continue
			}
			fmt.Fprintf(b, "  - %s: `%s` @ `%s`.\n", side.name, text(side.value.Repository), text(side.value.Revision))
			for i, source := range side.value.Sources {
				if compact && i == 4 {
					b.WriteString("  - More source revisions are in JSON.\n")
					break
				}
				fmt.Fprintf(b, "  - %s source %d: `%s` @ `%s`.\n", side.name, source.Position, text(source.Repository), text(source.Revision))
			}
		}
		fmt.Fprintf(b, "  - Required action: %s\n", text(violation.Action))
	}
	if limit < len(report.Policy.Violations) {
		b.WriteString("\nMore policy evidence is in JSON.\n")
	}
	b.WriteString("\n")
	for _, limitation := range report.Policy.Limitations {
		fmt.Fprintf(b, "- Limit: %s\n", text(limitation))
	}
	b.WriteString("\n")
}
