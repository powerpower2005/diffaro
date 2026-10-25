package security

import (
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// splitRedactor is the path matching of Redactor before it stopped splitting
// each path: the oracle for the equivalence tests below.
type splitRedactor struct{ r Redactor }

func (o splitRedactor) maskedWhole(p string) bool {
	if strings.Contains(p, "/helm/values") || strings.Contains(p, "/helm/parameters") || strings.Contains(p, "/plugin/env") {
		return true
	}
	for _, configured := range o.r.Paths {
		if p == configured || strings.HasPrefix(p, configured+"/") {
			return true
		}
	}
	return false
}

func (o splitRedactor) Sensitive(p string) bool {
	if o.maskedWhole(p) {
		return true
	}
	parts := strings.Split(p, "/")
	if splitMatchesSuffix(parts, referenceLeaves) || o.r.valueFiles && splitMatchesSuffix(parts, valueFileReferenceLeaves) {
		return false
	}
	for _, part := range parts {
		key := strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(part))
		if strings.Contains(key, "password") || strings.Contains(key, "secret") || strings.Contains(key, "token") || strings.Contains(key, "privatekey") || strings.Contains(key, "lastappliedconfiguration") || key == "authorization" || key == "clientkeydata" || key == "clientcertificatedata" {
			return true
		}
	}
	return false
}

func (o splitRedactor) ComparesListItems(p string) bool {
	return !o.Sensitive(p) || o.r.valueFiles && !o.maskedWhole(p)
}

func (o splitRedactor) referenceContainer(v any, p string) bool {
	switch v.(type) {
	case map[string]any, []any:
	default:
		return false
	}
	if o.maskedWhole(p) {
		return false
	}
	return splitMatchesSuffix(strings.Split(p, "/"), referenceContainers) || o.r.valueFiles && splitHoldsReference(v, p)
}

func splitHoldsReference(v any, p string) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, item := range x {
			if splitHoldsReference(item, p+"/"+EscapePointer(k)) {
				return true
			}
		}
		return false
	case []any:
		for i, item := range x {
			if splitHoldsReference(item, p+"/"+strconv.Itoa(i)) {
				return true
			}
		}
		return false
	}
	parts := strings.Split(p, "/")
	return splitMatchesSuffix(parts, referenceLeaves) || splitMatchesSuffix(parts, valueFileReferenceLeaves)
}

func splitMatchesSuffix(parts []string, patterns [][]string) bool {
	for _, pattern := range patterns {
		if len(parts) < len(pattern) {
			continue
		}
		tail := parts[len(parts)-len(pattern):]
		matched := true
		for i, want := range pattern {
			if want == "*" {
				if _, err := strconv.Atoi(tail[i]); err != nil {
					matched = false
					break
				}
			} else if tail[i] != want {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func (o splitRedactor) Value(v any, p string) any {
	if o.Sensitive(p) && !o.referenceContainer(v, p) {
		return Mask
	}
	switch x := v.(type) {
	case string:
		return o.r.Text(x)
	case map[string]any:
		if name, ok := x["name"].(string); ok && o.Sensitive("/"+name) {
			out := map[string]any{}
			for k, v := range x {
				if k == "value" {
					out[k] = Mask
				} else {
					out[k] = o.Value(v, p+"/"+EscapePointer(k))
				}
			}
			return out
		}
		out := map[string]any{}
		for k, v := range x {
			out[o.r.Text(k)] = o.Value(v, p+"/"+EscapePointer(k))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = o.Value(v, p+"/"+strconv.Itoa(i))
		}
		return out
	default:
		return v
	}
}

// equivalenceSegments covers every pattern element, list index forms that
// strconv.Atoi accepts or rejects, credential words in other cases and with
// separators, runes that strings.ToLower maps to ASCII letters, escaped
// tokens and configured path segments.
var equivalenceSegments = []string{
	"", "spec", "name", "key", "path", "property", "items", "data", "env", "value",
	"secretKeyRef", "secretRef", "volumes", "secret", "secretName", "imagePullSecrets", "tls",
	"remoteRef", "secretKey", "existingSecret", "helm", "values", "valuesObject", "parameters", "plugin",
	"0", "7", "12", "+1", "-3", "01", "+", "-", "1-", "1_0", "9223372036854775807", "9223372036854775808", "99999999999999999999", "x1", "0x1",
	"PASS-word", "Pass_Word", "API_TOKEN", "ToKen", "PRİVATEKEY", "private_key", "Authorization", "author-ization", "authorizationx",
	"client_key_data", "Client-Certificate-Data", "last-applied-configuration", "kubectl.kubernetes.io~1last-applied-configuration",
	"ſecret", "SECRETS", "public", "custom", "a~1b", "token~0", "\xffpassword", strings.Repeat("A", 80) + "Token",
}

func equivalenceRedactors() []Redactor {
	configured := Redactor{Paths: []string{"/spec/custom", "/data", "custom", ""}}
	return []Redactor{{}, Redactor{}.ForValueFiles(), configured, configured.ForValueFiles()}
}

// Path matching gives the same results as splitting each path, for every
// path of up to two segments from equivalenceSegments, every pattern with
// each "*" and then each element replaced by another segment, under a few
// prefixes, all with and without a leading slash, and random deeper paths.
func TestSensitiveMatchesSplitPaths(t *testing.T) {
	var paths []string
	for _, a := range equivalenceSegments {
		for _, b := range equivalenceSegments {
			paths = append(paths, "/"+a, "/"+a+"/"+b, a+"/"+b)
		}
	}
	for _, patterns := range [][][]string{referenceLeaves, valueFileReferenceLeaves, referenceContainers} {
		for _, pattern := range patterns {
			for _, segment := range equivalenceSegments {
				for i := range pattern {
					variant := append([]string(nil), pattern...)
					for j := range variant {
						if variant[j] == "*" {
							variant[j] = segment
						}
					}
					tail := strings.Join(variant, "/")
					variant[i] = segment
					for _, prefix := range []string{"", "/", "/spec", "/spec/custom", "/x/helm/values"} {
						paths = append(paths, prefix+tail, prefix+"/"+tail, prefix+"/"+strings.Join(variant, "/"))
					}
				}
			}
		}
	}
	random := rand.New(rand.NewSource(1))
	for range 20000 {
		var b strings.Builder
		for range 4 + random.Intn(5) {
			b.WriteString("/" + equivalenceSegments[random.Intn(len(equivalenceSegments))])
		}
		paths = append(paths, b.String())
	}
	leaves := []any{"fictional", map[string]any{}, []any{}}
	for _, r := range equivalenceRedactors() {
		o := splitRedactor{r}
		for _, p := range paths {
			if got, want := r.Sensitive(p), o.Sensitive(p); got != want {
				t.Fatalf("valueFiles=%t Paths=%q: Sensitive(%q) = %t, want %t", r.valueFiles, r.Paths, p, got, want)
			}
			if got, want := r.ComparesListItems(p), o.ComparesListItems(p); got != want {
				t.Fatalf("valueFiles=%t Paths=%q: ComparesListItems(%q) = %t, want %t", r.valueFiles, r.Paths, p, got, want)
			}
			for _, v := range leaves {
				if got, want := r.referenceContainer(v, p), o.referenceContainer(v, p); got != want {
					t.Fatalf("valueFiles=%t Paths=%q: referenceContainer(%T, %q) = %t, want %t", r.valueFiles, r.Paths, v, p, got, want)
				}
			}
		}
	}
}

// Value redacts random nested values exactly as the split implementation.
func TestValueMatchesSplitPaths(t *testing.T) {
	random := rand.New(rand.NewSource(2))
	var value func(depth int) any
	value = func(depth int) any {
		switch n := random.Intn(6); {
		case depth == 0 || n < 2:
			return "fictional-" + equivalenceSegments[random.Intn(len(equivalenceSegments))]
		case n < 4:
			m := map[string]any{}
			for range 1 + random.Intn(4) {
				m[equivalenceSegments[random.Intn(len(equivalenceSegments))]] = value(depth - 1)
			}
			if random.Intn(3) == 0 {
				m["name"] = equivalenceSegments[random.Intn(len(equivalenceSegments))]
			}
			return m
		default:
			l := make([]any, 1+random.Intn(3))
			for i := range l {
				l[i] = value(depth - 1)
			}
			return l
		}
	}
	prefixes := []string{"", "/spec", "/spec/template/spec/containers/0", "/spec/source/helm/valuesObject", "/spec/custom", "/externalSecrets/0"}
	for _, r := range equivalenceRedactors() {
		o := splitRedactor{r}
		for range 3000 {
			v, p := value(5), prefixes[random.Intn(len(prefixes))]
			if got, want := r.Value(v, p), o.Value(v, p); !reflect.DeepEqual(got, want) {
				t.Fatalf("valueFiles=%t Paths=%q: Value(%v, %q) = %v, want %v", r.valueFiles, r.Paths, v, p, got, want)
			}
		}
	}
}

func FuzzSensitiveMatchesSplitPaths(f *testing.F) {
	for _, s := range equivalenceSegments {
		f.Add("/spec/" + s + "/name")
	}
	f.Fuzz(func(t *testing.T, p string) {
		for _, r := range equivalenceRedactors() {
			o := splitRedactor{r}
			if got, want := r.Sensitive(p), o.Sensitive(p); got != want {
				t.Fatalf("valueFiles=%t Paths=%q: Sensitive(%q) = %t, want %t", r.valueFiles, r.Paths, p, got, want)
			}
			if got, want := r.referenceContainer(map[string]any{}, p), o.referenceContainer(map[string]any{}, p); got != want {
				t.Fatalf("valueFiles=%t Paths=%q: referenceContainer(%q) = %t, want %t", r.valueFiles, r.Paths, p, got, want)
			}
		}
	})
}
