package security

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const Mask = "[REDACTED]"

var authorization = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)([^\r\n]+)`)

// URL userinfo credentials: the password of
// scheme://user:password@ in any scheme, which may contain "@" up to the
// last one before the host, and an HTTP(S) user without a password, which
// holds a token as in https://token@host. Scheme, user, host and path stay.
var (
	urlPassword = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^/\s:@"'<>]*:)[^/\s"'<>]*@`)
	urlToken    = regexp.MustCompile(`(?i)(https?://)[^/\s:@"'<>]+@`)
)

// keySeparators is built once: strings.Replacer is safe for concurrent use and
// constructing it per path segment dominated redaction allocations.
var keySeparators = strings.NewReplacer("-", "", "_", "")

type Redactor struct {
	Secrets []string
	Paths   []string
	// Masked, when set, records the strings Value masks in rendered targets
	// (not in value files) for the run.
	Masked *MaskedValues
	// valueFiles is set by ForValueFiles.
	valueFiles bool
}

// ForValueFiles returns a redactor for the keys of repository value files.
// It also shows the reference leaves that name a Secret in Helm values,
// redacts an object or list under a sensitive key field by field when it
// holds such a leaf, and compares lists such as externalSecrets item by item
// with their values masked. Configured paths stay masked
// whole.
func (r Redactor) ForValueFiles() Redactor {
	r.valueFiles = true
	return r
}

func (r Redactor) Text(s string) string {
	if strings.Contains(s, "PRIVATE KEY-----") {
		return Mask
	}
	if mayHoldAuthorization(s) {
		s = authorization.ReplaceAllString(s, "${1}"+Mask)
	}
	if strings.Contains(s, "://") && strings.IndexByte(s, '@') >= 0 {
		s = urlPassword.ReplaceAllString(s, "${1}"+Mask+"@")
		s = urlToken.ReplaceAllString(s, "${1}"+Mask+"@")
	}
	for _, secret := range r.Secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, Mask)
		}
	}
	return strings.Map(func(c rune) rune {
		if unicode.IsControl(c) && c != '\n' && c != '\t' {
			return '�'
		}
		return c
	}, s)
}

// mayHoldAuthorization reports whether s contains "authorization" in any
// ASCII case, which every match of the authorization pattern does: under
// (?i) no letter of the word folds to a non-ASCII rune. The check avoids
// running the pattern on each string.
func mayHoldAuthorization(s string) bool {
	const word = "authorization"
	for i := 0; i+len(word) <= len(s); i++ {
		j := 0
		// Only a letter and its other ASCII case set to a lowercase letter.
		for j < len(word) && s[i+j]|0x20 == word[j] {
			j++
		}
		if j == len(word) {
			return true
		}
	}
	return false
}

// referenceContainer reports an object or list at a reference container path
// that only a built-in key makes sensitive; configured paths and Helm inputs
// stay masked whole. In value files an object or list that holds a reference
// leaf is one too.
func (r Redactor) referenceContainer(v any, p string) bool {
	switch v.(type) {
	case map[string]any, []any:
	default:
		return false
	}
	if r.maskedWhole(p) {
		return false
	}
	return matchesSuffix(p, referenceContainers) || r.valueFiles && holdsReference(v, p)
}

// holdsReference reports whether v, at p in a value file, has a leaf that
// names a Secret.
func holdsReference(v any, p string) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, item := range x {
			if holdsReference(item, p+"/"+EscapePointer(k)) {
				return true
			}
		}
		return false
	case []any:
		for i, item := range x {
			if holdsReference(item, p+"/"+strconv.Itoa(i)) {
				return true
			}
		}
		return false
	}
	return matchesSuffix(p, referenceLeaves) || matchesSuffix(p, valueFileReferenceLeaves)
}

// maskedWhole reports Helm inline inputs, plugin env and configured paths.
func (r Redactor) maskedWhole(p string) bool {
	// Parent charts may render Applications containing inline credentials.
	// Preserve that a value changed, but never expose Helm input values.
	return helmInput(p) || r.configuredPath(p)
}

// configuredPath reports a path at or under a configured sensitive path.
func (r Redactor) configuredPath(p string) bool {
	for _, configured := range r.Paths {
		if strings.HasPrefix(p, configured) && (len(p) == len(configured) || p[len(configured)] == '/') {
			return true
		}
	}
	return false
}

// helmInput reports a path under Helm inline inputs or plugin env.
func helmInput(p string) bool {
	return strings.Contains(p, "/helm/values") || strings.Contains(p, "/helm/parameters") || strings.Contains(p, "/plugin/env")
}

// ComparesListItems reports whether a list at p may be compared item by
// item, which shows its item names: a list at a path that is not sensitive,
// or in value files any list that is not masked whole.
func (r Redactor) ComparesListItems(p string) bool {
	return !r.Sensitive(p) || r.valueFiles && !r.maskedWhole(p)
}

func EscapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func (r Redactor) Sensitive(p string) bool {
	if r.maskedWhole(p) {
		return true
	}
	if matchesSuffix(p, referenceLeaves) || r.valueFiles && matchesSuffix(p, valueFileReferenceLeaves) {
		return false
	}
	for part := range strings.SplitSeq(p, "/") {
		if sensitiveKey(part) {
			return true
		}
	}
	return false
}

// sensitiveKey reports a path segment that names a credential once "-" and
// "_" are removed and it is lowercased. An ASCII segment is normalized in a
// buffer on the stack; any other uses strings.ToLower, which also maps
// runes such as the Kelvin sign to ASCII letters.
func sensitiveKey(part string) bool {
	var buf [64]byte
	key := buf[:0]
	for i := 0; i < len(part); i++ {
		c := part[i]
		switch {
		case c >= utf8.RuneSelf:
			return sensitiveWord(strings.ToLower(keySeparators.Replace(part)))
		case c == '-' || c == '_':
			continue
		case 'A' <= c && c <= 'Z':
			c += 'a' - 'A'
		}
		key = append(key, c)
	}
	return sensitiveWord(string(key))
}

func sensitiveWord(key string) bool {
	return strings.Contains(key, "password") || strings.Contains(key, "secret") || strings.Contains(key, "token") || strings.Contains(key, "privatekey") || strings.Contains(key, "lastappliedconfiguration") || key == "authorization" || key == "clientkeydata" || key == "clientcertificatedata"
}

// referenceLeaves are fields that name a Secret or one of its keys instead of
// holding a value. They are shown even under a sensitive key such
// as secretKeyRef. "*" matches a list index.
var referenceLeaves = [][]string{
	{"secretKeyRef", "name"}, {"secretKeyRef", "key"},
	{"secretRef", "name"},
	{"volumes", "*", "secret", "secretName"},
	{"volumes", "*", "secret", "items", "*", "key"}, {"volumes", "*", "secret", "items", "*", "path"},
	{"imagePullSecrets", "*", "name"},
	{"tls", "*", "secretName"},
	{"remoteRef", "key"}, {"remoteRef", "property"},
	{"data", "*", "secretKey"},
}

// valueFileReferenceLeaves are keys that name a Secret in Helm values, and
// ExternalSecret references written as a list of remoteRef items. A key such
// as secretKey alone stays masked: charts also use it for credentials, such
// as an S3 secret key.
var valueFileReferenceLeaves = [][]string{
	{"secretName"}, {"existingSecret"},
	{"remoteRef", "*", "key"}, {"remoteRef", "*", "property"}, {"remoteRef", "*", "secretKey"},
}

// referenceContainers are objects and lists that hold reference leaves. An
// object or list at such a path is redacted field by field instead of as a
// whole, so its reference leaves are shown and its other fields stay masked.
var referenceContainers = [][]string{
	{"secretKeyRef"}, {"secretRef"},
	{"volumes", "*", "secret"},
	{"volumes", "*", "secret", "items"}, {"volumes", "*", "secret", "items", "*"},
	{"imagePullSecrets"}, {"imagePullSecrets", "*"},
}

// matchesSuffix reports whether the last "/"-separated segments of p match
// one of patterns.
func matchesSuffix(p string, patterns [][]string) bool {
	for _, pattern := range patterns {
		if suffixMatches(p, pattern) {
			return true
		}
	}
	return false
}

// suffixMatches compares pattern with the last segments of p from the end,
// without splitting p.
func suffixMatches(p string, pattern []string) bool {
	end := len(p)
	for i := len(pattern) - 1; i >= 0; i-- {
		if end < 0 {
			// p has fewer segments than pattern.
			return false
		}
		start := strings.LastIndexByte(p[:end], '/') + 1
		segment := p[start:end]
		if pattern[i] == "*" {
			if !isIndex(segment) {
				return false
			}
		} else if segment != pattern[i] {
			return false
		}
		end = start - 1
	}
	return true
}

// isIndex reports whether strconv.Atoi accepts s. Only a sign and digits
// can parse, so Atoi, which allocates an error, runs only on those.
func isIndex(s string) bool {
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && (i > 0 || s[i] != '+' && s[i] != '-') {
			return false
		}
	}
	_, err := strconv.Atoi(s)
	return err == nil
}

func (r Redactor) Value(v any, p string) any {
	if r.Sensitive(p) && !r.referenceContainer(v, p) {
		// A key name such as automountServiceAccountToken makes a path
		// sensitive, but a boolean there holds no credential. Configured
		// paths and Helm inputs stay masked whole.
		if _, ok := v.(bool); ok && !r.maskedWhole(p) {
			return v
		}
		// Only configured paths are known credentials; a key name or a
		// Helm inline input is masked as a precaution and is recorded apart
		//.
		switch {
		case r.valueFiles:
		case r.configuredPath(p):
			r.Masked.Add(v)
		default:
			r.Masked.AddPrecaution(v)
		}
		return Mask
	}
	switch x := v.(type) {
	case string:
		return r.Text(x)
	case map[string]any:
		// Environment variable names carry sensitivity while their values sit at /value.
		if name, ok := x["name"].(string); ok && r.Sensitive("/"+name) {
			out := map[string]any{}
			for k, v := range x {
				if k == "value" {
					out[k] = Mask
				} else {
					out[k] = r.Value(v, p+"/"+EscapePointer(k))
				}
			}
			return out
		}
		out := map[string]any{}
		for k, v := range x {
			out[r.Text(k)] = r.Value(v, p+"/"+EscapePointer(k))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = r.Value(v, p+"/"+strconv.Itoa(i))
		}
		return out
	default:
		return v
	}
}
