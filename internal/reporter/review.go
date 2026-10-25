package reporter

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"

	"diffaro/internal/domain"
)

// What each format says about one Application in review lists: its
// namespace, boundary, resource summary, next check and anchor.

func appAnchor(id domain.ApplicationID) string {
	return fmt.Sprintf("app-%x", sha256.Sum256([]byte(id.String())))
}

func reviewNamespace(a domain.ApplicationResult) string {
	base, head := a.Namespace, a.Namespace
	if a.BaseTarget != nil {
		base = a.BaseTarget.Namespace
	}
	if a.HeadTarget != nil {
		head = a.HeadTarget.Namespace
	}
	if a.State == domain.StateAdded && a.BaseTarget == nil {
		base = head
	}
	if a.State == domain.StateRemoved && a.HeadTarget == nil {
		head = base
	}
	if base == head {
		return knownDestination(head)
	}
	return knownDestination(base) + " -> " + knownDestination(head) + " (base -> head)"
}

func reviewBoundary(a *appView) string {
	if a.SkipReason != "" {
		return "Skipped: " + a.SkipReason
	}
	if len(a.issues) > 0 {
		i := a.issues[0]
		return strings.TrimPrefix(labeled(" | ", "Stage", i.Stage, "Side", i.Side, "Reason", i.Reason), " | ")
	}
	return ""
}

// labeled joins "Label: value" for each label and non-empty value, each
// preceded by separator; an empty value leaves its label out.
func labeled(separator string, pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			b.WriteString(separator + pairs[i] + ": " + pairs[i+1])
		}
	}
	return b.String()
}

func reviewResources(a domain.ApplicationResult) string {
	groups := map[string]int{}
	for _, res := range a.Resources {
		if res.State != domain.StateUnchanged || resourceComparisonIncomplete(res) {
			groups[res.ID.Kind+" "+string(res.State)]++
		}
	}
	keys := slices.Sorted(maps.Keys(groups))
	values := []string{}
	shown, more := bounded(keys, 3)
	for _, k := range shown {
		values = append(values, fmt.Sprintf("%s x%d", k, groups[k]))
	}
	if more > 0 {
		values = append(values, fmt.Sprintf("+%d more kinds/states; JSON", more))
	}
	if len(values) == 0 {
		if a.Execution != domain.ExecutionComplete || a.State == domain.StateUnknown {
			return "targets unknown"
		}
		if a.Pruning != nil {
			return fmt.Sprintf("%d known pruning candidates", len(a.Pruning.Resources))
		}
		return "no resource changes observed"
	}
	return strings.Join(values, ", ")
}

func applicationReviewAction(a domain.ApplicationResult) string {
	if a.Execution.FailedOrDenied() {
		for _, issue := range a.Issues {
			if issue.Action != "" {
				return issue.Action
			}
		}
		return "Resolve the reported failure and rerun; independent results remain available."
	}
	if a.Execution == domain.ExecutionSkipped {
		return "Resolve the skip reason and rerun; targets and descendants may be unknown."
	}
	if a.PlaceholderInput != nil {
		return "Review the changes shown; this Application was rendered from placeholder values, so unchanged targets are unknown."
	}
	for _, res := range a.Resources {
		if resourceComparisonIncomplete(res) {
			return "Resolve incomplete target comparisons before relying on this branch."
		}
	}
	if a.State == domain.StateUnknown {
		return "Resolve graph coverage; Application presence is unknown."
	}
	if a.Pruning != nil {
		return fmt.Sprintf("Check prune policy, finalizers and cascade for %d known target candidates; live deletion is unknown.", len(a.Pruning.Resources))
	}
	if a.State == domain.StateRemoved {
		return "Pruning candidates unknown; inspect base target coverage and prune/cascade configuration."
	}
	if a.State == domain.StateAdded {
		return "Review new target resources and configuration; live readiness is unknown."
	}
	if a.State == domain.StateUnchanged && len(a.InputChanges) > 0 {
		return "PR inputs changed but rendered targets are unchanged; confirm the input change is meant to have no target effect."
	}
	if !appNeedsReview(a) {
		return unchangedAction
	}
	return defaultReviewAction
}

// defaultReviewAction is the action of a fully compared changed
// Application, and unchangedAction that of an unchanged one. Neither adds
// to the state and changes shown, so formats omit them (showsAction).
const (
	defaultReviewAction = "Review the changed target fields."
	unchangedAction     = "This Application's rendered targets are unchanged."
)

// showsAction reports an action that tells the reader more than the state.
func showsAction(action string) bool {
	return action != defaultReviewAction && action != unchangedAction
}
