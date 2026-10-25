package manifest

import (
	"errors"
	"io"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/security"
	"go.yaml.in/yaml/v3"
)

// ParseValues decodes the first YAML document of a Helm values file, as Helm
// does. Anchors and merge keys are allowed; non-string keys and excessive
// nesting are rejected. An empty document is an empty map.
func ParseValues(data []byte) (map[string]any, error) {
	var node yaml.Node
	if err := yaml.NewDecoder(strings.NewReader(string(data))).Decode(&node); err != nil {
		if errors.Is(err, io.EOF) {
			return map[string]any{}, nil
		}
		return nil, errors.New("values file is not valid YAML")
	}
	count := 0
	if !safeValuesNode(&node, 0, &count) {
		return nil, errors.New("values file has non-string keys or excessive nesting")
	}
	var values map[string]any
	if err := node.Decode(&values); err != nil {
		return nil, errors.New("values file is not a mapping with unique string keys")
	}
	if values == nil {
		values = map[string]any{}
	}
	return values, nil
}

// safeValuesNode is safeNode for values files: alias nodes are allowed
// because their anchors are checked where defined, and the YAML decoder
// bounds alias expansion.
func safeValuesNode(n *yaml.Node, depth int, count *int) bool {
	*count++
	if depth > 64 || *count > 200000 {
		return false
	}
	if n.Kind == yaml.AliasNode {
		return true
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode || (key.Tag != "!!str" && key.Tag != "!!merge") {
				return false
			}
		}
	}
	for _, child := range n.Content {
		if !safeValuesNode(child, depth+1, count) {
			return false
		}
	}
	return true
}

// CompareValues returns key-level changes between two values documents in
// stable key order. prefix is the document's JSON pointer in its owner and
// only decides redaction; returned paths are relative to the document.
func CompareValues(prefix string, base, head map[string]any, redactor security.Redactor) []domain.Change {
	changes := []domain.Change{}
	walk(prefix, nil, domain.CloneSpec(base), domain.CloneSpec(head), true, true, redactor, &changes)
	for i := range changes {
		changes[i].Path = strings.TrimPrefix(changes[i].Path, prefix)
		if changes[i].Path == "" {
			changes[i].Path = "/"
		}
	}
	return changes
}
