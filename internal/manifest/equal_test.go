package manifest

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

// jsonEqual must decide every pair as reflect.DeepEqual does, so replacing it
// in walk cannot change which changes are reported.
func TestJSONEqualMatchesDeepEqual(t *testing.T) {
	type named map[string]any
	shared := []any{math.NaN()}
	sharedMap := map[string]any{"x": math.NaN()}
	var nilMap map[string]any
	var nilList []any
	var nilPointer *int
	one := 1
	nested := func(leaf any) any {
		return map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "api", "image": leaf}}}}
	}
	values := []any{
		nil,
		nilMap,
		map[string]any{},
		map[string]any{"a": nil},
		map[string]any{"b": nil},
		map[string]any{"a": "1"},
		map[string]any{"a": json.Number("1")},
		map[string]any{"a": 1.0},
		map[string]any{"a": "1", "b": "2"},
		named{"a": "1"},
		nilList,
		[]any{},
		[]any{nil},
		[]any{"a", "b"},
		[]any{"b", "a"},
		[]any{"a"},
		[]any{[]any{}},
		[]any{[]any(nil)},
		[]any{map[string]any{}},
		[]any{map[string]any(nil)},
		"", "1", "true", "null",
		true, false,
		json.Number("1"), json.Number("1.0"), json.Number("1e0"),
		float64(1), float64(0), math.NaN(), math.Inf(1),
		int64(1), int(1), int32(1), uint(1),
		[]string{"a"}, []string(nil), map[string]string{"a": "1"},
		nilPointer, &one,
		nested("app:1"), nested("app:2"), nested(json.Number("1")), nested(nil),
		shared, sharedMap,
		[]any{math.NaN()}, map[string]any{"x": math.NaN()},
	}
	// Equal pairs built separately, so equality is not decided by identity.
	pairs := [][2]any{
		{nested("app:1"), nested("app:1")},
		{map[string]any{"a": []any{json.Number("1"), true, nil}}, map[string]any{"a": []any{json.Number("1"), true, nil}}},
		{[]any{map[string]any{"k": map[string]any{}}}, []any{map[string]any{"k": map[string]any{}}}},
		{[]string{"a"}, []string{"a"}},
		{shared, shared},
		{sharedMap, sharedMap},
		{shared[:0], shared[:0]},
	}
	for _, a := range values {
		for _, b := range values {
			pairs = append(pairs, [2]any{a, b})
		}
	}
	equal, unequal := 0, 0
	for _, p := range pairs {
		want := reflect.DeepEqual(p[0], p[1])
		if got := jsonEqual(p[0], p[1]); got != want {
			t.Errorf("jsonEqual(%#v, %#v) = %v, reflect.DeepEqual = %v", p[0], p[1], got, want)
		}
		if want {
			equal++
		} else {
			unequal++
		}
	}
	if equal < len(values) || unequal == 0 {
		t.Fatalf("table decides too few pairs each way: %d equal, %d unequal", equal, unequal)
	}
}
