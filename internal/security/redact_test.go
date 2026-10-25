package security

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"unicode"
)

func TestNestedAndConfiguredSecrets(t *testing.T) {
	r := Redactor{Secrets: []string{"fictional-known-credential"}, Paths: []string{"/spec/containers/0/custom"}}
	v := map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"custom": "fictional-custom", "env": []any{map[string]any{"name": "DATABASE_PASSWORD", "value": "fictional-env"}}, "public": "okay"}}}, "note": "fictional-known-credential", "annotation": "Authorization: Bearer fictional-header"}
	b, _ := json.Marshal(r.Value(v, ""))
	if strings.Contains(string(b), "fictional-") {
		t.Fatal("secret escaped redaction")
	}
	if !strings.Contains(string(b), "okay") {
		t.Fatal("non-sensitive data was discarded")
	}
	if r.Text("-----BEGIN PRIVATE KEY-----\nfictional-key\n-----END PRIVATE KEY-----") != Mask {
		t.Fatal("PEM key not masked")
	}
}

func redactionManifest(n int) map[string]any {
	items := make([]any, n)
	for i := range items {
		items[i] = map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": "app", "namespace": "team-a", "labels": map[string]any{"app.kubernetes.io/name": "app", "app.kubernetes.io/part-of": "shop"}},
			"spec": map[string]any{"replicas": 3, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{
				"name": "app", "image": "registry.example.test/app:1.0.0",
				"env": []any{map[string]any{"name": "LOG_LEVEL", "value": "info"}, map[string]any{"name": "API_TOKEN", "value": "fictional"}},
			}}}}},
		}
	}
	return map[string]any{"items": items}
}

func TestSensitiveKeyNormalizationMatchesFreshReplacer(t *testing.T) {
	var r Redactor
	parts := []string{"", "-", "_", "--__", "pass-word", "pass_word", "Private_Key", "private-_key", "client-key-data", "Client_Certificate_Data", "author-ization", "to-k_en", "secre--t", "last-applied-configuration", "public", "a-b_c", "x__-y"}
	for _, part := range parts {
		if got, want := keySeparators.Replace(part), strings.NewReplacer("-", "", "_", "").Replace(part); got != want {
			t.Fatalf("Replace(%q) = %q, want %q", part, got, want)
		}
	}
	for _, p := range []string{"/spec/pass-word", "/data/private_key", "/x/client-key-data", "/a/author_ization", "/metadata/annotations/kubectl.kubernetes.io~1last-applied-configuration"} {
		if !r.Sensitive(p) {
			t.Fatalf("%s not sensitive", p)
		}
	}
	if r.Sensitive("/spec/replicas") {
		t.Fatal("non-sensitive path masked")
	}
}

func TestRedactorConcurrentUse(t *testing.T) {
	r := Redactor{Secrets: []string{"fictional-known-credential"}}
	v := redactionManifest(5)
	want, _ := json.Marshal(r.Value(v, ""))
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				got, _ := json.Marshal(r.Value(v, ""))
				if string(got) != string(want) || strings.Contains(string(got), "fictional") {
					t.Error("concurrent redaction diverged")
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestSecretReferencesAreShownAndValuesStayMasked verifies that fields
// that name a Secret or one of its keys are shown; literal values under
// sensitive keys stay masked.
func TestSecretReferencesAreShownAndValuesStayMasked(t *testing.T) {
	podSpec := map[string]any{
		"imagePullSecrets": []any{map[string]any{"name": "registry-pull"}},
		"volumes":          []any{map[string]any{"name": "tls", "secret": map[string]any{"secretName": "web-tls", "items": []any{map[string]any{"key": "tls-cert-key", "path": "tls-cert.pem", "mode": "fictional-mode"}}, "defaultMode": "fictional-default-mode"}}},
		"containers": []any{map[string]any{"name": "app", "env": []any{
			map[string]any{"name": "SLACK_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "app-secrets", "key": "slack-token", "optional": "fictional-optional"}}},
			map[string]any{"name": "API_TOKEN", "value": "fictional-literal-token", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "api-secrets", "key": "api-token"}}},
		}, "envFrom": []any{map[string]any{"secretRef": map[string]any{"name": "shared-env"}}}}},
	}
	manifest := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{"spec": podSpec},
			"tls":      []any{map[string]any{"hosts": []any{"app.example.test"}, "secretName": "app-cert"}},
			"data":     []any{map[string]any{"secretKey": "DB_PASSWORD", "remoteRef": map[string]any{"key": "app", "property": "db-password"}}},
			// Literal values under sensitive keys, including shapes close to a reference.
			"apiToken":     "fictional-api-token",
			"secret":       "fictional-secret-string",
			"auth":         map[string]any{"secretKeyRef": "fictional-ref-string", "secretRef": map[string]any{"name": "auth-ref", "value": "fictional-ref-value"}},
			"tlsSecret":    map[string]any{"secretName": "fictional-not-a-tls-item"},
			"passwordFile": map[string]any{"name": "fictional-password-name"},
		},
	}
	b, _ := json.Marshal(Redactor{}.Value(manifest, ""))
	out := string(b)
	for _, want := range []string{"registry-pull", "web-tls", "tls-cert-key", "tls-cert.pem", "app-secrets", "slack-token", "api-secrets", "api-token", "shared-env", "app-cert", "DB_PASSWORD", "db-password", "auth-ref"} {
		if !strings.Contains(out, want) {
			t.Fatalf("reference %q was masked:\n%s", want, out)
		}
	}
	if strings.Contains(out, "fictional-") {
		t.Fatalf("a value escaped redaction:\n%s", out)
	}
}

// Helm inputs and configured sensitive paths stay masked whole, even when
// they have the shape of a reference.
func TestReferenceShapesInsideMaskedInputsStayMasked(t *testing.T) {
	ref := map[string]any{"name": "fictional-name", "key": "fictional-key"}
	r := Redactor{Paths: []string{"/spec/custom"}}
	for _, p := range []string{"/spec/source/helm/valuesObject/env/secretKeyRef", "/spec/source/helm/parameters/0/secretKeyRef", "/spec/custom/env/secretKeyRef"} {
		if got := r.Value(ref, p); got != Mask {
			t.Fatalf("%s was not masked whole: %v", p, got)
		}
		if leaf := p + "/name"; !r.Sensitive(leaf) {
			t.Fatalf("%s is not sensitive", leaf)
		}
	}
	// The diff compares maps field by field and redacts each changed leaf on
	// its own path, so leaves near a reference shape must stay sensitive.
	for _, leaf := range []string{"/spec/passwordFile/name", "/spec/auth/secretRef/value", "/spec/apiToken", "/spec/tlsSecret/secretName", "/spec/template/spec/volumes/0/secret/items/0/mode", "/spec/template/spec/volumes/0/secret/defaultMode", "/spec/tlsSecret/items/0/key", "/spec/env/0/valueFrom/secretKeyRef/optional"} {
		if !r.Sensitive(leaf) {
			t.Fatalf("%s is not sensitive", leaf)
		}
	}
	// A list index position accepts only digits.
	if !r.Sensitive("/spec/tls/primary/secretName") || r.Sensitive("/spec/tls/0/secretName") {
		t.Fatal("list index pattern matched a map key or missed an index")
	}
}

// In value files, keys that name a Secret and ExternalSecret references
// written as remoteRef lists are shown, an object or list under a sensitive
// key is redacted field by field, and other values stay masked. Other
// redactors, Helm inline inputs and configured paths are unchanged.
func TestValueFileReferencesAreShown(t *testing.T) {
	values := map[string]any{
		"slack": map[string]any{"enabled": true, "secretName": "app-secrets", "secretKey": "fictional-key-name"},
		"auth":  map[string]any{"existingSecret": "db-auth", "password": "fictional-password"},
		"externalSecrets": []any{map[string]any{"name": "fictional-entry", "type": "fictional-type", "remoteRef": []any{
			map[string]any{"key": "app/dev", "property": "SLACK_URL", "secretKey": "SLACK_URL_KEY", "version": "fictional-version"},
		}}},
		"s3": map[string]any{"secretKey": "fictional-s3-credential"},
		// No reference inside: the object stays masked whole.
		"vaultSecrets": map[string]any{"defaultVaultPath": "fictional-path", "secretKeys": []any{map[string]any{"secretKey": "fictional-secret-key"}}},
		"custom":       map[string]any{"secretName": "fictional-configured"},
	}
	r := Redactor{Paths: []string{"/custom"}}
	b, _ := json.Marshal(r.ForValueFiles().Value(values, ""))
	out := string(b)
	for _, want := range []string{"app-secrets", "db-auth", "app/dev", "SLACK_URL", "SLACK_URL_KEY"} {
		if !strings.Contains(out, want) {
			t.Fatalf("value file reference %q was masked:\n%s", want, out)
		}
	}
	if strings.Contains(out, "fictional-") || !strings.Contains(out, `"vaultSecrets":"[REDACTED]"`) {
		t.Fatalf("a value escaped redaction, or an object without references was redacted field by field:\n%s", out)
	}
	b, _ = json.Marshal(r.Value(values, ""))
	if out := string(b); strings.Contains(out, "app-secrets") || strings.Contains(out, "db-auth") || strings.Contains(out, "app/dev") {
		t.Fatalf("a redactor for manifests showed value file references:\n%s", out)
	}
	files := r.ForValueFiles()
	for _, p := range []string{"/spec/source/helm/values/slack", "/spec/source/helm/valuesObject/slack", "/custom"} {
		if got := files.Value(map[string]any{"secretName": "fictional-name"}, p); got != Mask {
			t.Fatalf("%s was not masked whole: %v", p, got)
		}
	}
	for _, tc := range []struct {
		r    Redactor
		p    string
		want bool
	}{
		{files, "/externalSecrets", true},
		{files, "/externalSecrets/0/remoteRef", true},
		{files, "/custom/list", false},
		{files, "/spec/source/helm/values/list", false},
		{r, "/externalSecrets", false},
		{r, "/spec/containers", true},
	} {
		if got := tc.r.ComparesListItems(tc.p); got != tc.want {
			t.Errorf("ComparesListItems(%s) = %t, want %t", tc.p, got, tc.want)
		}
	}
}

// The authorization pre-check in Text matches the word in ASCII case only.
// That finds every match of the (?i) pattern only while no letter of the
// word folds to a non-ASCII rune, as the Kelvin sign folds to k.
func TestAuthorizationLettersFoldOnlyToASCII(t *testing.T) {
	for _, c := range "authorization" {
		for f := unicode.SimpleFold(c); f != c; f = unicode.SimpleFold(f) {
			if f > unicode.MaxASCII {
				t.Fatalf("%q folds to non-ASCII %q", c, f)
			}
		}
	}
}

// textWithPattern is Text running the authorization pattern on every string.
func textWithPattern(r Redactor, s string) string {
	if strings.Contains(s, "PRIVATE KEY-----") {
		return Mask
	}
	s = authorization.ReplaceAllString(s, "${1}"+Mask)
	s = urlPassword.ReplaceAllString(s, "${1}"+Mask+"@")
	s = urlToken.ReplaceAllString(s, "${1}"+Mask+"@")
	for _, secret := range r.Secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, Mask)
		}
	}
	return strings.Map(func(c rune) rune {
		if unicode.IsControl(c) && c != '\n' && c != '\t' {
			return '\uFFFD'
		}
		return c
	}, s)
}

var authorizationTexts = []string{
	"", "Authorization: Bearer fictional", "AUTHORIZATION=fictional", "x-authorization :\tfictional\nnext",
	"aUtHoRiZaTiOn = fictional\r\nAuthorization: second", "authorization:", "authorization", "authorizatio: fictional",
	"uthorization: fictional", "authorization fictional", "author\u0130zation: fictional", "author\u0131zation: fictional",
	"\u212Aauthorization: fictional", "authori\u017Fation: fictional", "\xffauthorization:\xfe fictional", "auth\x00orization: fictional",
	"Authorization:\u00a0fictional", "@UTHORIZATION: fictional", "authorizationauthorization: fictional", "fictional-known-credential authorization=fictional",
	"https://user:fictional@example.com/x", "https://fictional@example.com", "@ https://example.com/a@b", "x://:@", "://a:b@", "HTTPS://T@H",
}

// The password of URL userinfo, and an HTTP(S) user without a password,
// which holds a token, are masked in every string; scheme, user, host and
// path stay visible.
func TestURLUserinfoCredentialsAreMasked(t *testing.T) {
	r := Redactor{}
	for in, want := range map[string]string{
		"--db=postgres://app:fictional-pass@db:5432/app":         "--db=postgres://app:" + Mask + "@db:5432/app",
		"https://x-access-token:fictional-pat@example.com/a.git": "https://x-access-token:" + Mask + "@example.com/a.git",
		"redis://:fictional-pass@cache:6379":                     "redis://:" + Mask + "@cache:6379",
		"postgres://app:fictional@pass@db/app":                   "postgres://app:" + Mask + "@db/app",
		"HTTPS://fictional-token@example.com/a.git":              "HTTPS://" + Mask + "@example.com/a.git",
		"http://fictional-token@localhost:8080":                  "http://" + Mask + "@localhost:8080",
		"a https://u:p1@one.example b https://u:p2@two.example":  "a https://u:" + Mask + "@one.example b https://u:" + Mask + "@two.example",
		"url: \"https://u:fictional@example.com\", owner: a@b":   "url: \"https://u:" + Mask + "@example.com\", owner: a@b",
		// Not credentials: a user without a password outside HTTP(S), a
		// mail address, an "@" after the host and a port.
		"ssh://git@example.com/org/repo.git":  "ssh://git@example.com/org/repo.git",
		"git@example.com:org/repo.git":        "git@example.com:org/repo.git",
		"owner@example.com":                   "owner@example.com",
		"https://example.com/users/@fiction":  "https://example.com/users/@fiction",
		"https://example.com:8443/path?q=1":   "https://example.com:8443/path?q=1",
		"image: registry.example.com/app@sha": "image: registry.example.com/app@sha",
	} {
		if got := r.Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
		if got := r.Value(in, "/spec/source/repoURL"); got != want {
			t.Errorf("Value(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTextMatchesPatternOnEveryString(t *testing.T) {
	r := Redactor{Secrets: []string{"fictional-known-credential"}}
	for _, s := range authorizationTexts {
		if got, want := r.Text(s), textWithPattern(r, s); got != want {
			t.Errorf("Text(%q) = %q, want %q", s, got, want)
		}
	}
}

func FuzzTextMatchesPatternOnEveryString(f *testing.F) {
	for _, s := range authorizationTexts {
		f.Add(s)
	}
	r := Redactor{Secrets: []string{"fictional-known-credential"}}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := r.Text(s), textWithPattern(r, s); got != want {
			t.Fatalf("Text(%q) = %q, want %q", s, got, want)
		}
	})
}

// A boolean under a credential-like key name, such as a ServiceAccount's
// automountServiceAccountToken, is shown: it holds no credential and is a
// setting reviewers need. Strings and numbers there, and booleans under
// configured paths or Helm inputs, stay masked.
func TestBooleanUnderSensitiveKeyNameIsShown(t *testing.T) {
	r := Redactor{Paths: []string{"/spec/flags"}}
	for _, tc := range []struct {
		path  string
		value any
		want  any
	}{
		{"/automountServiceAccountToken", true, true},
		{"/spec/enableSecretRotation", false, false},
		{"/automountServiceAccountToken", "true", Mask},
		{"/spec/tokenTTL", float64(3600), Mask},
		{"/spec/flags/rotateToken", true, Mask},
		{"/spec/source/helm/parameters/0/forceString", true, Mask},
	} {
		if got := r.Value(tc.value, tc.path); got != tc.want {
			t.Errorf("%s=%v: got %v, want %v", tc.path, tc.value, got, tc.want)
		}
	}
	// Inside an object the same rule applies per field.
	got := r.Value(map[string]any{"automountServiceAccountToken": false, "token": "fictional-secret-value"}, "").(map[string]any)
	if got["automountServiceAccountToken"] != false || got["token"] != Mask {
		t.Fatalf("object fields: %v", got)
	}
}
