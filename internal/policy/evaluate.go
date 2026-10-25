// Package policy evaluates explicitly configured policies against target diffs.
// It performs no provider calls and does not inspect live cluster state.
//
// These are Diffaro report policies (for example deny-resource-removal-v1); for
// Argo CD AppProject rules see package argopolicy.
package policy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"diffaro/internal/domain"
)

const (
	DenyResourceRemoval = "deny-resource-removal-v1"
	MaxRules            = 32
	MaxResources        = 16384
	MaxViolations       = 256
	MaxEvidenceBytes    = 2 << 20
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var kindName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)

func Validate(rules []domain.PolicyRule) error {
	if len(rules) > MaxRules {
		return fmt.Errorf("policies exceed the limit of %d rules", MaxRules)
	}
	seen := map[string]bool{}
	for _, rule := range rules {
		if len(rule.ID) > 64 || !identifier.MatchString(rule.ID) || seen[rule.ID] {
			return fmt.Errorf("policies require unique IDs of 1 to 64 identifier characters")
		}
		seen[rule.ID] = true
		if rule.Rule != DenyResourceRemoval {
			return fmt.Errorf("policies contain an unsupported rule; use deny-resource-removal-v1")
		}
		if len(rule.Match.Kind) > 128 || !kindName.MatchString(rule.Match.Kind) {
			return fmt.Errorf("policies.match.kind must be an exact Kubernetes kind")
		}
		for _, value := range []string{rule.Match.Group, rule.Match.Instance, rule.Match.Project, rule.Match.Namespace, rule.Match.Name} {
			if len(value) > 253 || (value != "" && !identifier.MatchString(value)) {
				return fmt.Errorf("policy selectors must be bounded exact identifiers; wildcards are unsupported")
			}
		}
	}
	return nil
}

// Apply runs after rendering and selection verification. Failure statuses take
// precedence over policy violations; independent, proven violations remain in
// the report. Callers must validate trusted rules before rendering.
//
// Stages: newEvaluation sorts the rules, checkRun records what the run as a
// whole leaves unproven, evaluateApplication checks each Application in
// identity order until the resource bound is reached, and record writes the
// sorted summary and status into the report.
func Apply(report *domain.Report, rules []domain.PolicyRule) {
	if len(rules) == 0 {
		return
	}
	e := newEvaluation(*report, rules)
	e.checkRun(*report)
	apps := report.Applications
	for _, i := range sortedByKey(len(apps), func(i int) string { return apps[i].ID.String() }) {
		if !e.evaluateApplication(apps[i]) {
			break
		}
	}
	e.record(report)
}

// evaluation is one policy run: the summary it builds and how much of each
// bound it has used.
type evaluation struct {
	revisions domain.RevisionPair
	// excludePreexisting: failures.excludePreexistingFromExit left
	// Applications already failing before the PR out of the exit code.
	excludePreexisting bool
	summary            domain.PolicySummary
	// keys holds the sort key of each violation, in summary.Violations order.
	keys []string
	// resources counts evaluated resources (MaxResources) and evidenceBytes
	// the encoded violations (MaxEvidenceBytes).
	resources, evidenceBytes int
}

func newEvaluation(report domain.Report, rules []domain.PolicyRule) *evaluation {
	e := &evaluation{revisions: report.Revisions, excludePreexisting: report.PreexistingFailuresExcluded > 0, summary: domain.PolicySummary{
		Evaluation: "complete", Scope: report.Coverage,
		Rules:       append([]domain.PolicyRule{}, rules...),
		Violations:  []domain.PolicyViolation{},
		Limitations: []string{"Policies evaluate selected rendered targets only; resource removal does not prove live deletion or an outage."},
	}}
	sort.Slice(e.summary.Rules, func(i, j int) bool { return e.summary.Rules[i].ID < e.summary.Rules[j].ID })
	return e
}

// markIncomplete marks the evaluation incomplete and records why, once.
func (e *evaluation) markIncomplete(reason string) {
	e.summary.Evaluation = "incomplete"
	for _, known := range e.summary.Limitations {
		if known == reason {
			return
		}
	}
	e.summary.Limitations = append(e.summary.Limitations, reason)
}

// checkRun records what the run as a whole leaves unproven: an incomplete
// comparison or revision selection, incomplete graph discovery, excluded
// preexisting failures and no compared Applications.
func (e *evaluation) checkRun(report domain.Report) {
	if report.Status != domain.StatusSuccess || report.Revisions.Base == "" || report.Revisions.Head == "" {
		e.markIncomplete("Comparison or revision selection did not complete; resolve reported failures and rerun.")
	}
	if report.Graph != nil && (!report.Graph.BaseComplete || !report.Graph.HeadComplete) && !onlyExcludedFailures(report) {
		e.markIncomplete("Graph discovery is incomplete; missing branches cannot prove target removal.")
	}
	if report.PreexistingFailuresExcluded > 0 {
		e.summary.Limitations = append(e.summary.Limitations, "Applications already failing before this PR are not evaluated (failures.excludePreexistingFromExit); removals in them are unknown on both revisions.")
	}
	if len(report.Applications) == 0 {
		e.markIncomplete("No Application targets were compared; verify selection and read permissions.")
	}
}

// evaluateApplication checks the removed resources of one Application, in
// identity order, against every rule. It returns false once the resource
// bound is exceeded, which ends the evaluation.
func (e *evaluation) evaluateApplication(app domain.ApplicationResult) bool {
	// Applications already failing before the PR, left out of the exit code
	// by the operator, are unknown on both revisions: the PR
	// cannot remove what neither side rendered, so they are not evaluated.
	if e.excludePreexisting && app.FailureOrigin == domain.FailurePreexisting {
		return true
	}
	if app.Execution != domain.ExecutionComplete || app.State == domain.StateUnknown || len(app.Issues) != 0 {
		e.markIncomplete("Some Application targets failed, were skipped or are unknown; their removals were not evaluated.")
		return true
	}
	diffs := app.Resources
	for _, d := range sortedByKey(len(diffs), func(i int) string { return diffs[i].ID.String() }) {
		resource := diffs[d]
		e.resources++
		if e.resources > MaxResources {
			e.markIncomplete("Policy resource limit exceeded; reduce comparison scope and rerun.")
			return false
		}
		if resource.Comparison != domain.ComparisonComplete || resource.State == domain.StateUnknown {
			e.markIncomplete("Some resource comparisons are incomplete; their removals were not evaluated.")
			continue
		}
		if resource.State != domain.StateRemoved {
			continue
		}
		for _, rule := range e.summary.Rules {
			if matches(rule.Match, app, resource.ID) {
				e.addViolation(rule, app, resource.ID)
			}
		}
	}
	return true
}

// addViolation records that rule forbids removing resource, within the
// violation and evidence bounds.
func (e *evaluation) addViolation(rule domain.PolicyRule, app domain.ApplicationResult, resource domain.ResourceID) {
	if len(e.summary.Violations) == MaxViolations {
		e.markIncomplete("Policy violation limit exceeded; reduce comparison scope and rerun.")
		return
	}
	violation := domain.PolicyViolation{
		PolicyID: rule.ID, RuleID: rule.Rule, Application: app.ID, Resource: resource,
		Base: revision(e.revisions, app, false), Head: revision(e.revisions, app, true),
		Evidence: domain.PolicyEvidence{Path: "targetResourcePresent", Before: true, After: false},
		Reason:   "The selected resource is present in the base target and absent from the head target.",
		Action:   "Restore the target resource or have the policy owner explicitly revise the trusted profile.",
	}
	encoded, err := json.Marshal(violation)
	if err != nil || e.evidenceBytes+len(encoded) > MaxEvidenceBytes {
		e.markIncomplete("Policy evidence byte limit exceeded; reduce comparison scope and rerun.")
		return
	}
	e.evidenceBytes += len(encoded)
	e.summary.Violations = append(e.summary.Violations, violation)
	e.keys = append(e.keys, strings.Join([]string{violation.PolicyID, violation.Application.String(), violation.Resource.String()}, "\x00"))
}

// record sorts the violations and limitations and writes the summary, the
// violation lines and the resulting status into the report.
func (e *evaluation) record(report *domain.Report) {
	summary := e.summary
	summary.Violations = make([]domain.PolicyViolation, 0, len(e.summary.Violations))
	for _, i := range sortedByKey(len(e.keys), func(i int) string { return e.keys[i] }) {
		summary.Violations = append(summary.Violations, e.summary.Violations[i])
	}
	sort.Strings(summary.Limitations)
	report.Policy = &summary
	report.PolicyViolations = []string{}
	for _, violation := range summary.Violations {
		report.PolicyViolations = append(report.PolicyViolations, fmt.Sprintf("%s (%s): %s target resource %s removed", violation.PolicyID, violation.RuleID, violation.Application.String(), violation.Resource.String()))
	}
	if report.Status == domain.StatusSuccess {
		if summary.Evaluation != "complete" {
			report.Status = domain.StatusPartialFailure
		} else if len(summary.Violations) != 0 {
			report.Status = domain.StatusPolicyViolation
		}
	}
}

// sortedByKey returns the indexes 0..n-1 ordered by key(i), computing each
// key once. sort.Slice moves the indexes exactly as it would move the
// elements, so the order, ties included, is that of sorting the elements.
func sortedByKey(n int, key func(int) string) []int {
	keys := make([]string, n)
	order := make([]int, n)
	for i := range n {
		keys[i], order[i] = key(i), i
	}
	sort.Slice(order, func(i, j int) bool { return keys[order[i]] < keys[order[j]] })
	return order
}

func matches(selector domain.PolicySelector, app domain.ApplicationResult, resource domain.ResourceID) bool {
	project := app.Project
	if app.BaseTarget != nil {
		project = app.BaseTarget.Project
	}
	return (selector.Instance == "" || selector.Instance == app.ID.Instance) &&
		(selector.Project == "" || selector.Project == project) &&
		selector.Group == resource.Group && selector.Kind == resource.Kind &&
		(selector.Namespace == "" || selector.Namespace == resource.Namespace) &&
		(selector.Name == "" || selector.Name == resource.Name)
}

func revision(pair domain.RevisionPair, app domain.ApplicationResult, head bool) domain.PolicyRevision {
	target, sources, sha := app.BaseTarget, app.BaseSources, pair.Base
	if head {
		target, sources, sha = app.HeadTarget, app.HeadSources, pair.Head
		if app.State == domain.StateRemoved {
			return domain.PolicyRevision{Absent: true}
		}
	}
	if target != nil {
		return domain.PolicyRevision{Repository: target.Repository, Revision: target.Revision, Sources: target.Sources}
	}
	return domain.PolicyRevision{Repository: pair.Repository, Revision: sha, Sources: sources}
}

// onlyExcludedFailures reports that every failure in the report belongs to
// an Application excluded as already failing before the PR. Graph discovery
// is then incomplete only below those Applications: render failures, limits,
// cycles and unknown presence each leave a failed Application or a report
// issue of their own.
func onlyExcludedFailures(report domain.Report) bool {
	if report.PreexistingFailuresExcluded == 0 {
		return false
	}
	excluded := map[domain.ApplicationID]bool{}
	for _, app := range report.Applications {
		switch {
		case app.FailureOrigin == domain.FailurePreexisting:
			excluded[app.ID] = true
		case app.Execution != domain.ExecutionComplete || app.State == domain.StateUnknown || len(app.Issues) != 0:
			return false
		}
	}
	for _, issue := range report.Issues {
		if issue.Application == nil || !excluded[*issue.Application] {
			return false
		}
	}
	return true
}
