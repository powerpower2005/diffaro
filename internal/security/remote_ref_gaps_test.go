package security

import "testing"

// A ClusterExternalSecret nests ExternalSecret data under externalSecretSpec,
// a sensitive key. remoteRef key and property name the external secret entry,
// so they stay visible under that ancestor, while a literal value under the
// same ancestor stays masked.
func TestRemoteRefLeavesUnderSensitiveAncestorAreShown(t *testing.T) {
	r := Redactor{}
	for p, v := range map[string]string{
		"/spec/externalSecretSpec/data/0/remoteRef/key":      "app",
		"/spec/externalSecretSpec/data/0/remoteRef/property": "db-password",
	} {
		if r.Sensitive(p) {
			t.Fatalf("%s is sensitive", p)
		}
		if got := r.Value(v, p); got != v {
			t.Fatalf("%s: reference %q shown as %v", p, v, got)
		}
	}
	for _, p := range []string{"/spec/externalSecretSpec/data/0/remoteRef/conversionStrategy", "/spec/externalSecretSpec/target/template/data/password"} {
		if got := r.Value("fictional-literal", p); got != Mask {
			t.Fatalf("%s: literal value shown as %v", p, got)
		}
	}
}
