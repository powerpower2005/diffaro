// Package security redacts secrets and configured sensitive paths in values
// and text (Redactor). Every encoder and log path must pass output through it.
package security

import (
	"encoding/base64"
	"maps"
	"strings"
	"sync"
)

// minEmbeddedMaskedBytes is the shortest value treated as part of a longer
// masked string, such as a password inside a masked connection URL. Shorter
// values are matched only when equal, so common short words stay visible.
const minEmbeddedMaskedBytes = 8

// MaskedValues collects, for one run, the strings that redaction masked in
// rendered targets: values under sensitive or configured paths and Secret
// payloads. Value file key changes mask the same strings where they reappear
// with an ordinary key name. It is kept in memory only and is safe
// for concurrent use.
//
// Values masked as a precaution rather than as a known credential are
// recorded apart with AddPrecaution: those under a sensitive key name, which
// often name a reference such as a Secret key, and Helm inline inputs and
// plugin env. Value file changes mask them too, but Credentials leaves them
// out for resource diffs.
type MaskedValues struct {
	mu     sync.Mutex
	values map[string]bool
	inputs map[string]bool
}

// Add records every non-empty string in v as a credential: a Secret payload
// or a value under a configured sensitive path. Use AddSecretData for Secret
// data values, which also records their base64 decoding.
func (m *MaskedValues) Add(v any) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values == nil {
		m.values = map[string]bool{}
	}
	m.add(m.values, v)
}

// AddPrecaution records every non-empty string in v as a precautionary
// mask: a value under a sensitive key name, a Helm inline input or plugin env.
func (m *MaskedValues) AddPrecaution(v any) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inputs == nil {
		m.inputs = map[string]bool{}
	}
	m.add(m.inputs, v)
}

// Credentials returns a copy of the strings recorded with Add and
// AddSecretData, without those recorded with AddPrecaution.
func (m *MaskedValues) Credentials() *MaskedValues {
	out := &MaskedValues{}
	if m == nil {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out.values = maps.Clone(m.values)
	return out
}

// AddSecretData records a Secret data value and its base64 decoding.
func (m *MaskedValues) AddSecretData(v any) {
	m.Add(v)
	if s, ok := v.(string); ok {
		if decoded, err := base64.StdEncoding.DecodeString(s); err == nil {
			m.Add(string(decoded))
		}
	}
}

func (m *MaskedValues) add(set map[string]bool, v any) {
	switch x := v.(type) {
	case string:
		if x != "" && x != Mask {
			set[x] = true
		}
	case map[string]any:
		for _, item := range x {
			m.add(set, item)
		}
	case []any:
		for _, item := range x {
			m.add(set, item)
		}
	}
}

// Empty reports whether no string was recorded, so Hides never reports one.
func (m *MaskedValues) Empty() bool {
	if m == nil {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.values) == 0 && len(m.inputs) == 0
}

// Hides reports whether s equals a masked string or, when it is at least
// minEmbeddedMaskedBytes long, appears inside one.
func (m *MaskedValues) Hides(s string) bool {
	if m == nil || s == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return hides(m.values, s) || hides(m.inputs, s)
}

func hides(set map[string]bool, s string) bool {
	if set[s] {
		return true
	}
	if len(s) < minEmbeddedMaskedBytes {
		return false
	}
	for masked := range set {
		if strings.Contains(masked, s) {
			return true
		}
	}
	return false
}

// MaskIn returns v with every string that Hides reports replaced by Mask,
// and whether it replaced any.
func (m *MaskedValues) MaskIn(v any) (any, bool) {
	switch x := v.(type) {
	case string:
		if m.Hides(x) {
			return Mask, true
		}
		return x, false
	case map[string]any:
		out, changed := make(map[string]any, len(x)), false
		for k, item := range x {
			masked, hit := m.MaskIn(item)
			out[k], changed = masked, changed || hit
		}
		return out, changed
	case []any:
		out, changed := make([]any, len(x)), false
		for i, item := range x {
			masked, hit := m.MaskIn(item)
			out[i], changed = masked, changed || hit
		}
		return out, changed
	}
	return v, false
}
