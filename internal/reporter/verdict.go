package reporter

import (
	"fmt"
	"strings"

	"diffaro/internal/domain"
)

// statusLabels name each status for the verdict headline.
var statusLabels = map[domain.Status]string{
	domain.StatusPartialFailure:       "partial failure",
	domain.StatusPolicyViolation:      "policy violation",
	domain.StatusFailed:               "failed",
	domain.StatusDenied:               "access denied",
	domain.StatusCancelled:            "cancelled",
	domain.StatusInvalidConfiguration: "invalid configuration",
}

// verdictKind names what a verdict line states, so a format selects lines by
// kind instead of by their text.
type verdictKind int

const (
	verdictCounted     verdictKind = iota // the failures the exit code counts
	verdictChanges                        // Applications added, changed or removed
	verdictNotAnalyzed                    // skipped Applications
	verdictPolicies                       // policy evaluation
	verdictNotCounted                     // failures failures.excludePreexistingFromExit leaves out
	verdictPreexisting                    // every counted failure predates the PR
	verdictRules                          // where the exit code rules are
)

type verdictLine struct {
	kind verdictKind
	text string
}

// verdict explains the status and exit code from the report itself: what was
// counted, what was not and why.
type verdict struct {
	// label names the status, such as "changes found".
	label string
	exit  int
	lines []verdictLine
}

// result names the status label and the exit code, as "changes found (exit
// 1)".
func (v verdict) result() string { return fmt.Sprintf("%s (exit %d)", v.label, v.exit) }

// headline states the result.
func (v verdict) headline() string { return "Verdict: " + v.result() }

// text returns the headline and every line, as terminal output, console
// diagnostics and the full verdict sections print them.
func (v verdict) text() []string {
	out := []string{v.headline()}
	for _, line := range v.lines {
		out = append(out, line.text)
	}
	return out
}

// verdictLines returns the verdict text (verdict.text) of a report.
func verdictLines(r domain.Report) []string { return verdictFrom(r, countReport(r)).text() }

// verdictFrom builds the verdict from the report and its counts.
func verdictFrom(r domain.Report, c reportCounts) verdict {
	var skipped []domain.ApplicationResult
	for _, app := range r.Applications {
		if app.Execution == domain.ExecutionSkipped {
			skipped = append(skipped, app)
		}
	}
	preflight := r.Coverage == domain.CoverageConnectionPreflight
	label := statusLabels[r.Status]
	if label == "" && preflight {
		// doctor without --render compares nothing.
		label = "read access checked; targets were not rendered"
	}
	if label == "" {
		label = "no changes"
		if r.ChangesFound {
			label = "changes found"
		} else if len(skipped) > 0 {
			// A skipped Application's resource changes are unknown, so the
			// run did not show that nothing changed.
			label = "no changes in analyzed Applications"
		}
	}
	v := verdict{label: label, exit: r.ExitCode()}
	add := func(kind verdictKind, text string) { v.lines = append(v.lines, verdictLine{kind, text}) }
	excluded := r.PreexistingFailuresExcluded > 0
	counts := map[string]int{}
	failedApps := map[domain.ApplicationID]bool{}
	for _, app := range r.Applications {
		if !app.Execution.FailedOrDenied() {
			continue
		}
		failedApps[app.ID] = true
		origin := app.FailureOrigin
		switch {
		case origin == domain.FailurePreexisting && excluded:
			counts["excluded"]++
		case app.Execution == domain.ExecutionDenied:
			counts["denied"]++
		case origin == domain.FailureIntroduced || origin == domain.FailurePreexisting || origin == domain.FailureInputsChanged || origin == domain.FailureTransient:
			counts[string(origin)]++
		default:
			counts["other"]++
		}
	}
	// Run-level issues are counted as the Issues sections group them: one
	// cause at base and head is one issue.
	var runLevel []domain.Issue
	for _, issue := range r.Issues {
		if issue.Application == nil || !failedApps[*issue.Application] {
			runLevel = append(runLevel, issue)
		}
	}
	runIssues := len(domain.GroupIssues(runLevel))
	var counted []string
	for _, part := range []struct {
		key, text string
	}{
		{string(domain.FailureIntroduced), "%d Application(s) broken by this PR"},
		{string(domain.FailurePreexisting), "%d Application(s) already failing before this PR"},
		{string(domain.FailureInputsChanged), "%d Application(s) already failing whose inputs this PR changes"},
		{string(domain.FailureTransient), "%d Application(s) that may pass on retry"},
		{"denied", "%d Application(s) denied access"},
		{"other", "%d Application(s) failed for other causes"},
	} {
		if counts[part.key] > 0 {
			counted = append(counted, fmt.Sprintf(part.text, counts[part.key]))
		}
	}
	if runIssues > 0 {
		counted = append(counted, fmt.Sprintf("%d run-level issue(s)", runIssues))
	}
	if len(counted) == 0 {
		add(verdictCounted, "- Counted failures: none")
	} else {
		add(verdictCounted, "- Counted failures: "+strings.Join(counted, ", "))
	}
	if preflight {
		add(verdictChanges, "- Changes: not compared; run doctor --render --pr NUMBER or diff to render and compare targets")
	} else {
		add(verdictChanges, fmt.Sprintf("- Changes: %d Application(s) added, changed or removed", c.appsStateChanged))
	}
	if len(skipped) > 0 {
		var names []string
		shown, more := bounded(skipped, 3)
		for _, app := range shown {
			name := app.ID.String()
			if app.SkipReason != "" {
				name += " (" + boundRunes(app.SkipReason, 320) + ")"
			}
			names = append(names, name)
		}
		if more > 0 {
			names = append(names, fmt.Sprintf("%d more in the report", more))
		}
		add(verdictNotAnalyzed, fmt.Sprintf("- Not analyzed: %d Application(s) skipped, resource changes unknown: %s", len(skipped), strings.Join(names, "; ")))
	}
	if r.Policy != nil {
		add(verdictPolicies, fmt.Sprintf("- Policies: evaluation %s, %d violation(s)", r.Policy.Evaluation, len(r.Policy.Violations)))
	}
	if excluded {
		add(verdictNotCounted, fmt.Sprintf("- Not counted: %d Application(s) already failing before this PR (failures.excludePreexistingFromExit); their resource changes are unknown", r.PreexistingFailuresExcluded))
	} else if r.Status == domain.StatusPartialFailure && counts[string(domain.FailurePreexisting)] > 0 && len(counted) == 1 {
		add(verdictPreexisting, "- Every counted failure predates this PR. failures.excludePreexistingFromExit would leave such failures out of the exit code; it is off, so they count")
	}
	add(verdictRules, "- Rules: see Exit codes in the Diffaro reference (docs/reference.md#exit-codes)")
	return v
}

func boundRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// VerdictLines returns the verdict for console diagnostics.
func VerdictLines(r domain.Report) []string { return verdictLines(r) }

// writeVerdict prints the verdict as a Markdown section.
func writeVerdict(b *strings.Builder, v verdict) {
	fmt.Fprintf(b, "**%s**\n\n", prText(v.headline(), 120))
	for _, line := range v.lines {
		fmt.Fprintf(b, "%s\n", prText(line.text, 1200))
	}
	b.WriteString("\n")
}
