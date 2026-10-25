package reporter

import (
	"fmt"
	"strconv"
	"strings"

	"diffaro/internal/domain"
)

// Phrases and labels the human-readable formats share.

const hiddenText = domain.HiddenChange

// stateSymbols mark Application, resource and graph link states in counts
// and lists; a state without one reads as unknown.
var stateSymbols = map[domain.State]string{domain.StateAdded: "+", domain.StateChanged: "~", domain.StateRemoved: "-", domain.StateUnchanged: "=", domain.StateUnknown: "?"}

func stateSymbol(state domain.State) string {
	if symbol, ok := stateSymbols[state]; ok {
		return symbol
	}
	return stateSymbols[domain.StateUnknown]
}

// changeStates are the states counts name, in the order every format lists
// them; countedStates add unknown.
var (
	changeStates  = []domain.State{domain.StateAdded, domain.StateChanged, domain.StateRemoved}
	countedStates = []domain.State{domain.StateAdded, domain.StateChanged, domain.StateRemoved, domain.StateUnknown}
)

// stateCounts joins the counts of states with sep, each after its symbol
// when marked, such as "+1 / ~2 / -0 / ?0".
func stateCounts(counts map[domain.State]int, states []domain.State, marked bool, sep string) string {
	var b strings.Builder
	for i, state := range states {
		if i > 0 {
			b.WriteString(sep)
		}
		if marked {
			b.WriteString(stateSymbol(state))
		}
		b.WriteString(strconv.Itoa(counts[state]))
	}
	return b.String()
}

// absentText stands for the side an added or removed value does not have,
// so it reads apart from an explicit null.
const absentText = "<absent>"

// pruningLiveUnknown and pruningUnknown state what pruning evidence does not
// show.
const (
	pruningLiveUnknown = "Candidates are rendered targets; live deletion is unknown."
	pruningUnknown     = "Pruning candidates: unknown (no pruning evidence available)"
)

// writePruningCandidates writes a pruning headline, up to limit known target
// candidates followed by more, and what the candidates do not show.
func writePruningCandidates(b *strings.Builder, headline string, ids []domain.ResourceID, limit int, more string, label func(domain.ResourceID) string) {
	b.WriteString(headline)
	for i, id := range ids {
		if i == limit {
			b.WriteString(more)
			break
		}
		fmt.Fprintf(b, "- Known target candidate: `%s`\n", label(id))
	}
	b.WriteString("\n" + pruningLiveUnknown + "\n\n")
}

// accessFields names a required access in the order Markdown formats print
// it; text escapes and bounds a value to a width.
func accessFields(access *domain.AccessRequirement, text func(value string, width int) string) []string {
	fields := []string{"credential env `" + text(access.CredentialEnv, 128) + "`", "system `" + text(access.System, 80) + "`", "instance `" + text(access.Instance, 180) + "`"}
	if access.Resource != "" {
		fields = append(fields, "resource `"+text(access.Resource, 80)+"`")
	}
	return append(fields, "action `"+text(access.Action, 40)+"`", "target `"+text(access.Target, 240)+"`")
}

func issueLabel(issue domain.Issue) string {
	if issue.ReasonCode != "" {
		return issue.Code + "/" + issue.ReasonCode
	}
	return issue.Code
}

// plural names a count with its noun.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// bounded returns the first limit items and how many items follow them.
func bounded[T any](items []T, limit int) ([]T, int) {
	if len(items) <= limit {
		return items, 0
	}
	return items[:limit], len(items) - limit
}

// unknownPathsText names up to five fields that were not compared because
// they differed between two renders of identical inputs.
func unknownPathsText(res domain.ResourceDiff, quote func(string) string) string {
	return "Not compared, differs between two renders of identical inputs: " + quotedPaths(res.UnknownPaths, quote)
}

// incompleteText says why a resource comparison is incomplete and what
// would complete it, or "" for a complete comparison.
func incompleteText(res domain.ResourceDiff, quote func(string) string) string {
	switch {
	case len(res.UnknownPaths) > 0:
		return unknownPathsText(res, quote)
	case res.IncompleteReason == domain.IncompleteMaskedSecret:
		return "Not compared: Argo CD's Application API masks Secret values, so whether they changed is unknown. Local Helm or repo-server compare them without showing values."
	case res.IncompleteReason == domain.IncompletePartialTarget:
		return "Not compared: the other side rendered only partially, so this resource's presence there is unknown."
	}
	return ""
}

// placeholderText explains an Application rendered from a desired spec whose
// fields differed between two renders of identical inputs.
func placeholderText(p *domain.PlaceholderInput, quote func(string) string) string {
	return fmt.Sprintf("Rendered from placeholder values: %s of %s differ between two renders of identical inputs. Changes shown are observed; resources are not proven unchanged.", quotedPaths(p.Paths, quote), quote(p.Application.String()))
}

// quotedPaths names up to five paths and counts the others.
func quotedPaths(paths []string, quote func(string) string) string {
	shown, more := bounded(paths, 5)
	names := []string{}
	for _, p := range shown {
		names = append(names, quote(p))
	}
	if more > 0 {
		names = append(names, fmt.Sprintf("%d more in JSON", more))
	}
	return strings.Join(names, ", ")
}

func resourceLabel(id domain.ResourceID) string {
	group := id.Group
	if group == "" {
		group = "core"
	}
	return id.Cluster + "/" + group + "/" + id.Kind + "/" + id.Namespace + "/" + id.Name
}

func resourceName(id domain.ResourceID) string {
	if id.Namespace == "" {
		return id.Name
	}
	return id.Namespace + "/" + id.Name
}

// itemPath shows a change path with the name of each list item it passes
// through (domain.NamedPath).
func itemPath(change domain.Change) string {
	return domain.NamedPath(change.Path, change.ItemNames)
}

// wholeResource reports whether a change adds or removes a whole resource.
func wholeResource(change domain.Change) bool {
	return change.Path == "/" && (change.Operation == domain.OperationAdd || change.Operation == domain.OperationRemove)
}

// highlightFields are the familiar target settings shown for a whole added
// or removed manifest, in order; the terminal also shows Application
// sources.
var highlightFields = []struct {
	path         string
	terminalOnly bool
}{{"spec/replicas", false}, {"spec/type", false}, {"spec/selector", false}, {"spec/ports", false}, {"spec/source", true}, {"spec/sources", true}, {"spec/destination", false}}

// containerFields are the container lists whose images are shown for a
// whole added or removed manifest.
var containerFields = []string{"spec/template/spec/containers", "spec/template/spec/initContainers", "spec/jobTemplate/spec/template/spec/containers", "spec/containers"}

// wholeView is the value of a whole added or removed resource and, for a
// manifest, the familiar settings in it.
type wholeView struct {
	value any
	// side is "Head" or "Base", mark "+" or "-".
	side, mark string
	// manifest is value as an object, when it is one (isManifest).
	manifest   map[string]any
	isManifest bool
	// fields are the highlightFields the manifest sets, in order;
	// containers are the containerFields lists it has, in order.
	fields     []highlight
	containers [][]any
}

type highlight struct {
	path         string
	terminalOnly bool
	value        any
}

func newWholeView(change domain.Change) wholeView {
	w := wholeView{value: change.After, side: "Head", mark: "+"}
	if change.Operation == domain.OperationRemove {
		w.value, w.side, w.mark = change.Before, "Base", "-"
	}
	w.manifest, w.isManifest = w.value.(map[string]any)
	if !w.isManifest {
		return w
	}
	for _, field := range highlightFields {
		if v, found := objectField(w.manifest, field.path); found {
			w.fields = append(w.fields, highlight{field.path, field.terminalOnly, v})
		}
	}
	for _, path := range containerFields {
		if v, found := objectField(w.manifest, path); found {
			if containers, ok := v.([]any); ok {
				w.containers = append(w.containers, containers)
			}
		}
	}
	return w
}

func objectField(object map[string]any, path string) (any, bool) {
	var current any = object
	for _, part := range strings.Split(path, "/") {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

// resolvedSuffix names the reference a revision was resolved from at run
// time, or "" when it was not resolved.
func resolvedSuffix(requested string, quote func(string) string) string {
	if requested == "" {
		return ""
	}
	return " (resolved from " + quote(requested) + " at run time)"
}
