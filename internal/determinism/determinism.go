// Package determinism compares two renders of identical inputs, as local
// Helm with an external executable and the repo-server backend do. Renders
// are paired by resource identity, and a field whose value
// differs between them is replaced with domain.RenderUnstable so it is not
// compared (Compare). DecodeDocument reads a YAML document
// like Argo CD does: a repeated mapping key keeps its last value.
package determinism

import (
	"encoding/json"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"

	"diffaro/internal/domain"

	"go.yaml.in/yaml/v3"
)

// parse is targetDocuments; tests count its calls.
var parse = targetDocuments

// Compare compares two renders of the same inputs, parsing each once.
//
// same reports whether they describe the same targets. Some Helm builds emit
// documents of one kind in a varying order; the comparison is identity-based
// like the diff itself, so that order is ignored. Repeated documents of one
// identity must keep their order, because the last occurrence wins
// . Comments are ignored. Unparseable output never compares equal
// unless byte-identical.
//
// When the renders differ, marked pairs their documents by resource identity
// and replaces each field whose value differs with domain.RenderUnstable
// . It is a stream of JSON documents, which YAML readers accept. ok
// is false when the outputs do not describe the same resources, such as a
// name with a random suffix, or cannot be parsed.
func Compare(first, second string) (marked string, same, ok bool) {
	if first == second {
		return "", true, true
	}
	a, okA := parse(first)
	b, okB := parse(second)
	if !okA || !okB {
		return "", false, false
	}
	if reflect.DeepEqual(a, b) {
		return "", true, true
	}
	marked, ok = markUnstable(a, b)
	return marked, false, ok
}

// markUnstable is the marked output of Compare for the parsed documents of
// two renders.
func markUnstable(a, b map[string][]string) (string, bool) {
	if len(a) != len(b) {
		return "", false
	}
	var out strings.Builder
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	for _, identity := range slices.Sorted(maps.Keys(a)) {
		if len(a[identity]) != len(b[identity]) {
			return "", false
		}
		for i := range a[identity] {
			x, okX := decodeCanonical(a[identity][i])
			y, okY := decodeCanonical(b[identity][i])
			if !okX || !okY {
				return "", false
			}
			out.WriteString("---\n")
			if encoder.Encode(unstable(x, y)) != nil {
				return "", false
			}
		}
	}
	return out.String(), true
}

// decodeCanonical decodes a document from targetDocuments, keeping numbers
// as written.
func decodeCanonical(doc string) (any, bool) {
	var v any
	decoder := json.NewDecoder(strings.NewReader(doc))
	decoder.UseNumber()
	return v, decoder.Decode(&v) == nil
}

// unstable returns a with every value that differs from b replaced by
// domain.RenderUnstable. Objects with the same keys and lists of the same
// length are compared member by member; otherwise the whole value differs.
func unstable(a, b any) any {
	switch x := a.(type) {
	case map[string]any:
		if y, ok := b.(map[string]any); ok && len(x) == len(y) {
			out := make(map[string]any, len(x))
			for k, v := range x {
				w, ok := y[k]
				if !ok {
					return domain.RenderUnstable
				}
				out[k] = unstable(v, w)
			}
			return out
		}
	case []any:
		if y, ok := b.([]any); ok && len(x) == len(y) {
			out := make([]any, len(x))
			for i := range x {
				out[i] = unstable(x[i], y[i])
			}
			return out
		}
	}
	if reflect.DeepEqual(a, b) {
		return a
	}
	return domain.RenderUnstable
}

// targetDocuments groups canonical JSON documents by resource identity,
// preserving the order of documents within each identity.
func targetDocuments(raw string) (map[string][]string, bool) {
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	out := map[string][]string{}
	for {
		obj, err := DecodeDocument(decoder)
		if err == io.EOF {
			return out, true
		}
		if err != nil {
			return nil, false
		}
		if len(obj) == 0 {
			continue
		}
		meta, _ := obj["metadata"].(map[string]any)
		identity, err := json.Marshal([]any{obj["apiVersion"], obj["kind"], meta["namespace"], meta["name"]})
		if err != nil {
			return nil, false
		}
		canonical, err := json.Marshal(obj)
		if err != nil {
			return nil, false
		}
		out[string(identity)] = append(out[string(identity)], string(canonical))
	}
}

// DecodeDocument reads the next YAML document like Argo CD does: a mapping
// key repeated within one object keeps its last value instead of failing.
// It returns io.EOF after the last document.
func DecodeDocument(decoder *yaml.Decoder) (map[string]any, error) {
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return nil, err
	}
	keepLastKeys(&node)
	var obj map[string]any
	if err := node.Decode(&obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// keepLastKeys removes earlier occurrences of repeated mapping keys.
func keepLastKeys(node *yaml.Node) {
	if node.Kind == yaml.MappingNode {
		last := map[string]int{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			if key := node.Content[i]; key.Kind == yaml.ScalarNode {
				last[key.Value] = i
			}
		}
		kept := node.Content[:0]
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind == yaml.ScalarNode && last[key.Value] != i {
				continue
			}
			kept = append(kept, key, node.Content[i+1])
		}
		node.Content = kept
	}
	for _, child := range node.Content {
		keepLastKeys(child)
	}
}
