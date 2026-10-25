package domain

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
)

type Change struct {
	Path      string    `json:"path"`
	Operation Operation `json:"operation"`
	Before    any       `json:"before"`
	After     any       `json:"after"`
	Redacted  bool      `json:"redacted,omitempty"`
	// ItemNames holds the name of each list item the path passes through,
	// outermost first, when lists are compared by item name.
	ItemNames []string `json:"itemNames,omitempty"`
}
type ResourceDiff struct {
	ID         ResourceID `json:"id"`
	State      State      `json:"state"`
	Changes    []Change   `json:"changes"`
	Comparison Comparison `json:"comparison"`
	// ScopeSource is "argocd-observed" when the namespace was inferred from
	// Argo CD's observed resource scope instead of target evidence.
	ScopeSource string `json:"scopeSource,omitempty"`
	// UnknownPaths lists the JSON pointers of fields whose rendered value
	// differed between two renders of identical inputs; they are not
	// compared.
	UnknownPaths []string `json:"unknownPaths,omitempty"`
	// IncompleteReason names why Comparison is incomplete: Argo CD masked
	// Secret values, fields differed between renders, or one side rendered
	// only partially.
	IncompleteReason string `json:"incompleteReason,omitempty"`
	// LineHunks show a changed resource's changes as lines of its redacted
	// YAML with one neighbouring block of context on each side.
	// Changes stay the compared facts; hunks are absent when the resource
	// is a Secret or its text is too large to diff.
	LineHunks []LineHunk `json:"lineHunks,omitempty"`
}

// ResourceDiff.IncompleteReason values.
const (
	IncompleteMaskedSecret   = "argocd-masked-secret"
	IncompleteUnstableFields = "unstable-fields"
	IncompletePartialTarget  = "partial-target"
)

// Hidden reports a replaced value that redaction hides on both sides; the
// reader learns that it changed, not what it changed to.
func (c Change) Hidden() bool {
	return c.Operation == OperationReplace && c.Redacted && reflect.DeepEqual(c.Before, c.After)
}

// HiddenChange describes a value that changed while redaction hides it on
// both sides.
const HiddenChange = "value changed (hidden)"

// LineHunk is one part of a resource's YAML line diff.
type LineHunk struct {
	// Path is the common path of the changed lines, with list items named
	// as in change paths shown to reviewers, such as
	// /spec/template/spec/containers[api]/env.
	Path string `json:"path"`
	// BaseStart and HeadStart are the 1-based YAML line numbers of the
	// first line at each revision.
	BaseStart int `json:"baseStart"`
	HeadStart int `json:"headStart"`
	// Lines start with ' ' (context), '-' (base only) or '+' (head only).
	Lines []string `json:"lines"`
}

// RenderUnstable replaces a rendered field whose value differed between two
// renders of identical inputs, such as a generated password. It is
// not a value the chart produced.
const RenderUnstable = "<differs between identical renders>"

// UnstablePaths returns the sorted JSON pointers, below p, of the values in v
// that are RenderUnstable.
func UnstablePaths(p string, v any) []string {
	// The sentinel is rare, so the walk keeps the segments leading to the
	// current value and builds a pointer only for a match.
	type segment struct {
		key   string
		index int // set for a list item
		item  bool
	}
	var out []string
	var stack []segment
	pointer := func() string {
		var b strings.Builder
		b.WriteString(p)
		for _, s := range stack {
			b.WriteByte('/')
			if s.item {
				b.WriteString(strconv.Itoa(s.index))
			} else {
				b.WriteString(strings.ReplaceAll(strings.ReplaceAll(s.key, "~", "~0"), "/", "~1"))
			}
		}
		return b.String()
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if x == RenderUnstable {
				out = append(out, pointer())
			}
		case map[string]any:
			for k, item := range x {
				stack = append(stack, segment{key: k})
				walk(item)
				stack = stack[:len(stack)-1]
			}
		case []any:
			for i, item := range x {
				stack = append(stack, segment{index: i, item: true})
				walk(item)
				stack = stack[:len(stack)-1]
			}
		}
	}
	walk(v)
	sort.Strings(out)
	return out
}

// InputChange is a render input of an Application that changes between base
// and head. It is evidence shown next to the resource diff; it does
// not claim which resource field a key produced.
type InputChange struct {
	// Kind is valueFile (key-level diff), chartFile (path only) or
	// inlineValues (spec helm.values; values stay masked).
	Kind string `json:"kind"`
	// Repository is set for repository files; Path is the repository path or,
	// for inlineValues, the JSON pointer in the Application spec.
	Repository string `json:"repository,omitempty"`
	Path       string `json:"path"`
	State      State  `json:"state"`
	// Comparison is complete, incomplete (Reason explains) or path-only.
	Comparison Comparison `json:"comparison"`
	Reason     string     `json:"reason,omitempty"`
	Changes    []Change   `json:"changes"`
}
