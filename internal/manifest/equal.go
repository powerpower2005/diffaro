package manifest

import (
	"encoding/json"
	"reflect"
)

// jsonEqual reports whether a and b are equal exactly as reflect.DeepEqual
// reports it, without reflection for the types of decoded JSON and YAML
// values: map[string]any, []any, string, bool, nil, json.Number, float64,
// int64 and int. Any other type is compared by reflect.DeepEqual. As there,
// a nil map or slice differs from an empty one, and a map or slice equals
// itself without comparing its elements. Values must not be cyclic.
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || (x == nil) != (y == nil) || len(x) != len(y) {
			return false
		}
		if reflect.ValueOf(x).UnsafePointer() == reflect.ValueOf(y).UnsafePointer() {
			return true
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !jsonEqual(xv, yv) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || (x == nil) != (y == nil) || len(x) != len(y) {
			return false
		}
		if len(x) > 0 && &x[0] == &y[0] {
			return true
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case json.Number:
		y, ok := b.(json.Number)
		return ok && x == y
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	case int64:
		y, ok := b.(int64)
		return ok && x == y
	case int:
		y, ok := b.(int)
		return ok && x == y
	}
	return reflect.DeepEqual(a, b)
}
