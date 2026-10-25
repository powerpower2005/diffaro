package security

import (
	"encoding/base64"
	"testing"
)

// Collected strings hide equal values and, from minEmbeddedMaskedBytes on,
// values embedded in a masked string; Secret data is also recorded decoded.
func TestMaskedValuesHideEqualAndEmbeddedStrings(t *testing.T) {
	var m MaskedValues
	m.Add("postgres://app:fictional-db-pass@db:5432/app")
	m.Add(map[string]any{"nested": []any{"fictional-api-key", true}})
	m.AddSecretData(base64.StdEncoding.EncodeToString([]byte("fictional-token-value")))
	m.Add(Mask)
	for s, want := range map[string]bool{
		"fictional-api-key":     true,  // equal
		"fictional-db-pass":     true,  // inside a masked URL
		"fictional-token-value": true,  // decoded Secret data
		"db":                    false, // too short to match inside
		"app":                   false,
		Mask:                    false,
		"":                      false,
		"unrelated-long-value":  false,
	} {
		if got := m.Hides(s); got != want {
			t.Errorf("Hides(%q) = %t, want %t", s, got, want)
		}
	}
	masked, hit := m.MaskIn(map[string]any{"key": "fictional-api-key", "list": []any{"keep", "fictional-db-pass"}, "n": float64(3)})
	got := masked.(map[string]any)
	if !hit || got["key"] != Mask || got["list"].([]any)[0] != "keep" || got["list"].([]any)[1] != Mask || got["n"] != float64(3) {
		t.Fatalf("MaskIn = %v %t", got, hit)
	}
	var none *MaskedValues
	if none.Hides("fictional-api-key") {
		t.Fatal("a nil collector hides nothing")
	}
}

// Value records what it masks in rendered targets, not in value files.
func TestRedactorRecordsMaskedTargetValues(t *testing.T) {
	m := &MaskedValues{}
	r := Redactor{Paths: []string{"/spec/image"}, Masked: m}
	r.Value("fictional-registry/app:fictional-tag", "/spec/image")
	r.Value("fictional-password-value", "/data/password")
	r.ForValueFiles().Value("fictional-value-file-secret", "/password")
	if !m.Hides("fictional-registry/app:fictional-tag") || !m.Hides("fictional-password-value") || m.Hides("fictional-value-file-secret") {
		t.Fatalf("recorded = %v", m.values)
	}
}

// Helm inline inputs and plugin env, masked whole as a precaution, are
// recorded apart: value file changes still hide them, but Credentials keeps
// only credentials (configured paths, Secret payloads) for resource diffs; a
// value masked by its key name, which often names a reference, is a
// precaution too. A configured path under a Helm input is a credential.
func TestRedactorRecordsHelmInlineInputsApart(t *testing.T) {
	m := &MaskedValues{}
	r := Redactor{Paths: []string{"/spec/source/helm/parameters/1"}, Masked: m}
	r.Value("image: fictional-registry/app:fictional-tag\n", "/spec/source/helm/values")
	r.Value(map[string]any{"host": "fictional-host.example.invalid"}, "/spec/source/helm/valuesObject")
	r.Value([]any{map[string]any{"name": "tag", "value": "1.4.3"}}, "/spec/source/helm/parameters")
	r.Value(map[string]any{"name": "configured", "value": "fictional-configured"}, "/spec/source/helm/parameters/1")
	r.Value("fictional-env-value", "/spec/source/plugin/env/0/value")
	r.Value("fictional-password-value", "/data/password")
	m.AddSecretData("fictional-secret-payload")
	credentials := m.Credentials()
	for s, input := range map[string]bool{
		"fictional-registry/app:fictional-tag": true,
		"fictional-host.example.invalid":       true,
		"1.4.3":                                true,
		"fictional-env-value":                  true,
		"fictional-configured":                 false,
		"fictional-password-value":             true,
		"fictional-secret-payload":             false,
	} {
		if !m.Hides(s) {
			t.Errorf("Hides(%q) = false; value file changes must still hide it", s)
		}
		if got := credentials.Hides(s); got == input {
			t.Errorf("Credentials().Hides(%q) = %t, want %t", s, got, !input)
		}
	}
	if !(&MaskedValues{}).Credentials().Empty() || !((*MaskedValues)(nil)).Credentials().Empty() {
		t.Fatal("an empty or nil collector has credentials")
	}
	only := &MaskedValues{}
	only.AddPrecaution("fictional-input")
	if only.Empty() || !only.Credentials().Empty() {
		t.Fatal("inputs alone: the collector is not empty, its credentials are")
	}
}
