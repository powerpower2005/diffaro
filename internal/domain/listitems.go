package domain

// ItemKeys are the fields that identify list items, tried in order: Kubernetes names, Prometheus alerting and recording
// rules, and ExternalSecret entries. A group of several fields matches when
// each item sets exactly one of them.
var ItemKeys = [][]string{{"name"}, {"alert", "record"}, {"secretKey"}}

// ItemKey returns the item's value for exactly one of keys, or "".
func ItemKey(item any, keys []string) string {
	_, value := ItemKeyField(item, keys)
	return value
}

// ListItemKeys returns the first ItemKeys group under which every item of
// both lists, a base and a head list or one list and nil, has a unique,
// non-empty value, with each list's map from that value to the item's index.
// It returns nil groups and maps when no group names every item. Change
// paths and YAML line paths use the same item keys.
func ListItemKeys(a, b []any) ([]string, map[string]int, map[string]int) {
	for _, keys := range ItemKeys {
		aIndex, aOK := itemIndex(a, keys)
		if !aOK {
			continue
		}
		if bIndex, bOK := itemIndex(b, keys); bOK {
			return keys, aIndex, bIndex
		}
	}
	return nil, nil, nil
}

// itemIndex maps each item's value for keys to its index, or reports false
// when an item has no unique, non-empty value.
func itemIndex(items []any, keys []string) (map[string]int, bool) {
	index := make(map[string]int, len(items))
	for i, item := range items {
		name := ItemKey(item, keys)
		if name == "" {
			return nil, false
		}
		if _, duplicate := index[name]; duplicate {
			return nil, false
		}
		index[name] = i
	}
	return index, true
}

// ItemName returns the field and value that name a list item under the
// first ItemKeys group it matches, or "", "".
func ItemName(item any) (string, string) {
	for _, keys := range ItemKeys {
		if field, value := ItemKeyField(item, keys); value != "" {
			return field, value
		}
	}
	return "", ""
}

// ItemKeyField returns the field and value of the item's exactly one of
// keys, or "", "".
func ItemKeyField(item any, keys []string) (string, string) {
	object, _ := item.(map[string]any)
	field, found := "", ""
	for _, key := range keys {
		value, ok := object[key].(string)
		if !ok {
			continue
		}
		if found != "" || value == "" {
			return "", ""
		}
		field, found = key, value
	}
	return field, found
}
