// Package yamlview prints values as the YAML that reports show (Text) and
// gives the path of each printed line (Lines). Keys are sorted, the field
// that names a list item (domain.ItemKeys) comes first, numbers are printed
// as written and the indent is two spaces, so line diffs and field views of
// the same value read alike.
package yamlview

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/security"
	"go.yaml.in/yaml/v3"
)

// Text returns the YAML of a value, ending with a newline. Each list item
// starts with the field of the first domain.ItemKeys group it matches.
func Text(value any) (string, bool) {
	return print(node(value, nil, nil, ""))
}

// ItemText returns the YAML of a list item, with the field that names it
// first: the field whose value is name, as in the item's change path, or
// else the field of the first domain.ItemKeys group it matches.
func ItemText(item any, name string) (string, bool) {
	field, _ := domain.ItemName(item)
	for _, keys := range domain.ItemKeys {
		if f, value := domain.ItemKeyField(item, keys); name != "" && value == name {
			field = f
			break
		}
	}
	return print(node(item, nil, nil, field))
}

func print(n *yaml.Node) (string, bool) {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if encoder.Encode(n) != nil || encoder.Close() != nil {
		return "", false
	}
	return out.String(), true
}

// Lines returns the YAML lines of a value and the path of each line, as
// segments: "/key" (escaped as a JSON pointer token), "[name]" for a list
// item named by domain.ItemKeys and "/index" for another item. A line inside
// a multi-line string has the path of the string. other is the value of the
// other revision, or nil: a list's items are named by the key group chosen
// from it and the matching list of other (domain.ListItemKeys), as change
// paths name them. In a list without such a group, each item is named by
// the first group it matches. An item's naming field is printed first.
func Lines(value, other any) ([]string, [][]string, bool) {
	// node records the names of each []any list in the order it builds
	// them, which is the order the walk below meets them in the printed
	// text.
	var lists [][]itemName
	text, ok := print(node(value, other, &lists, ""))
	if !ok {
		return nil, nil, false
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	// Line numbers are known only after parsing the printed text, which
	// keeps the structure and key order of value.
	var doc yaml.Node
	if yaml.Unmarshal([]byte(text), &doc) != nil || len(doc.Content) != 1 {
		return nil, nil, false
	}
	paths := make([][]string, len(lines))
	// o is the value at the same named path in other, or nil.
	var walk func(n *yaml.Node, v, o any, p []string)
	walk = func(n *yaml.Node, v, o any, p []string) {
		switch n.Kind {
		case yaml.MappingNode:
			object, _ := v.(map[string]any)
			otherObject, _ := o.(map[string]any)
			for i := 0; i+1 < len(n.Content); i += 2 {
				key := n.Content[i]
				cp := append(slices.Clip(p), "/"+security.EscapePointer(key.Value))
				set(paths, key.Line, cp)
				walk(n.Content[i+1], object[key.Value], otherObject[key.Value], cp)
			}
		case yaml.SequenceNode:
			items, isList := v.([]any)
			var names []itemName
			if isList {
				names, lists = lists[0], lists[1:]
			}
			for i, item := range n.Content {
				var value any
				var name itemName
				if i < len(items) {
					value, name = items[i], names[i]
				}
				segment := "/" + strconv.Itoa(i)
				if name.name != "" {
					segment = "[" + name.name + "]"
				}
				cp := append(slices.Clip(p), segment)
				set(paths, item.Line, cp)
				walk(item, value, name.other, cp)
			}
		}
	}
	walk(doc.Content[0], value, other, nil)
	for i := 1; i < len(paths); i++ {
		if paths[i] == nil {
			paths[i] = paths[i-1]
		}
	}
	return lines, paths, true
}

// itemName is how a list item is printed and named: field comes first and
// name names its path; other is the matching item of the other revision.
type itemName struct {
	field, name string
	other       any
}

// itemNames names the items of a list. With both, the key group is chosen
// from the list and other, the other revision's value at the same path, as
// change paths choose it (domain.ListItemKeys). Without both, or when no
// group fits, each item is named by the first group it matches.
func itemNames(items []any, other any, both bool) []itemName {
	names := make([]itemName, len(items))
	if both {
		otherItems, _ := other.([]any)
		if keys, _, otherIndex := domain.ListItemKeys(items, otherItems); keys != nil {
			for i, item := range items {
				field, name := domain.ItemKeyField(item, keys)
				names[i] = itemName{field: field, name: name}
				if j, shared := otherIndex[name]; shared {
					names[i].other = otherItems[j]
				}
			}
			return names
		}
	}
	for i, item := range items {
		names[i].field, names[i].name = domain.ItemName(item)
	}
	return names
}

func set(paths [][]string, line int, p []string) {
	if line >= 1 && line <= len(paths) {
		paths[line-1] = p
	}
}

// oldBools and base60 are the YAML 1.1 forms that go.yaml.in/yaml/v3 quotes
// when it encodes a Go string.
var (
	oldBools = map[string]bool{"y": true, "Y": true, "yes": true, "Yes": true, "YES": true, "on": true, "On": true, "ON": true, "n": true, "N": true, "no": true, "No": true, "NO": true, "off": true, "Off": true, "OFF": true}
	base60   = regexp.MustCompile(`^[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+(?:\.[0-9_]*)?$`)
)

// node builds the YAML node of a value, with field, when set, first. A list
// item's naming field (itemNames) comes first, so the item's first line
// names it. With lists, other is the other revision's value at the same
// path, and the names of each []any list are appended to lists in the order
// the lists are built.
func node(value, other any, lists *[][]itemName, field string) *yaml.Node {
	switch x := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if _, ok := x[field]; ok && field != "" {
			keys = append([]string{field}, slices.DeleteFunc(keys, func(k string) bool { return k == field })...)
		}
		otherObject, _ := other.(map[string]any)
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, k := range keys {
			n.Content = append(n.Content, stringNode(k), node(x[k], otherObject[k], lists, ""))
		}
		return n
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		names := itemNames(x, other, lists != nil)
		if lists != nil {
			*lists = append(*lists, names)
		}
		for i, name := range names {
			n.Content = append(n.Content, node(x[i], name.other, lists, name.field))
		}
		return n
	case json.Number:
		// Reports decode numbers as json.Number; print them as written,
		// neither quoted nor rounded.
		tag := "!!int"
		if strings.ContainsAny(string(x), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: string(x)}
	case string:
		return stringNode(x)
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(x)}
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	}
	// Other types, such as Go numbers in hand-built values, take the
	// encoder's own representation.
	var n yaml.Node
	if n.Encode(value) != nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	}
	return &n
}

// stringNode builds a string key or value. The encoder quotes a string that
// YAML 1.2 would read as another type and prints a multi-line string as a
// literal block; strings that YAML 1.1 reads as a bool or a sexagesimal
// number are quoted too, as the encoder does for Go strings.
func stringNode(s string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
	if oldBools[s] || base60.MatchString(s) {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}
