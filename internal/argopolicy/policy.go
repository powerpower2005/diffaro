package argopolicy

import (
	"net/url"
	"regexp"
	"strings"

	"diffaro/internal/domain"
	"github.com/gobwas/glob"
)

var sshPolicyURL = regexp.MustCompile(`^(ssh://)?([^/:]*?)@[^@]+$`)

// repositoryURL mirrors Argo CD v3.5.3's Git normalization for AppProject
// source policies.
// This does not alias different protocols for discovery or credential routing.
func repositoryURL(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if sshPolicyURL.MatchString(value) && !strings.HasPrefix(value, "ssh://") {
		value = "ssh://" + strings.Replace(value, ":", "/", 1)
	}
	value = strings.TrimSuffix(value, ".git")
	u, err := url.Parse(value)
	if err != nil {
		return "", unsupportedPolicy("invalid repository policy URL")
	}
	return strings.TrimPrefix(u.String(), "ssh://"), nil
}

func unsupportedPolicy(reason string) error {
	return domain.Fail(domain.CodeUnsupportedProjectPolicy, "permission", reason, "correct the AppProject rule or use a backend that supports its policy", nil)
}

func Pattern(pattern, value string, separators ...rune) (bool, error) {
	if pattern == "!*" {
		return false, unsupportedPolicy("AppProject does not permit a !* rule")
	}
	if len(pattern) > 2048 {
		return false, unsupportedPolicy("project pattern exceeds the supported length")
	}
	negative := strings.HasPrefix(pattern, "!")
	if negative {
		pattern = strings.TrimPrefix(pattern, "!")
	}
	var matched bool
	if pattern == "*" {
		matched = true
	} else {
		compiled, err := glob.Compile(pattern, separators...)
		if err != nil {
			return false, unsupportedPolicy("invalid project glob pattern")
		}
		matched = compiled.Match(value)
	}
	if negative {
		return !matched, nil
	}
	return matched, nil
}

func SourceAllowed(raw any, repository string) (bool, error) {
	rules, ok := raw.([]any)
	if !ok || len(rules) > 1024 {
		return false, unsupportedPolicy("source repository rules are missing or exceed the supported count")
	}
	value, err := repositoryURL(repository)
	if err != nil {
		return false, err
	}
	anyMatch := false
	for _, item := range rules {
		pattern, ok := item.(string)
		if !ok {
			return false, unsupportedPolicy("repository rule must be a string")
		}
		negative := strings.HasPrefix(pattern, "!")
		pattern, err = repositoryURL(strings.TrimPrefix(pattern, "!"))
		if err != nil {
			return false, err
		}
		if negative {
			pattern = "!" + pattern
		}
		matched, err := Pattern(pattern, value, '/')
		if err != nil {
			return false, err
		}
		if !matched && negative {
			return false, nil
		}
		anyMatch = anyMatch || matched
	}
	return anyMatch, nil
}

func DestinationAllowed(raw any, destination map[string]any) (bool, error) {
	rules, ok := raw.([]any)
	if !ok || len(rules) > 1024 {
		return false, unsupportedPolicy("destination rules are missing or exceed the supported count")
	}
	allowed := false
	for _, item := range rules {
		rule, ok := item.(map[string]any)
		if !ok {
			return false, unsupportedPolicy("destination rule must be an object")
		}
		matched := map[string]bool{}
		for _, key := range []string{"name", "server", "namespace"} {
			pattern := domain.String(rule, key)
			if raw, exists := rule[key]; exists {
				if _, ok := raw.(string); !ok {
					return false, unsupportedPolicy("destination selectors must be strings")
				}
			}
			if key != "namespace" && domain.String(destination, key) == "" {
				continue
			}
			var err error
			matched[key], err = Pattern(pattern, domain.String(destination, key))
			if err != nil {
				return false, err
			}
		}
		name, server, namespace := matched["name"], matched["server"], matched["namespace"]
		// Preserve the upstream destination deny precedence, including name
		// exclusions and server-scoped namespace exclusions.
		switch {
		case (server || name) && namespace:
			allowed = true
		case (!name && strings.HasPrefix(domain.String(rule, "name"), "!")) || (!server && strings.HasPrefix(domain.String(rule, "server"), "!") && namespace):
			return false, nil
		case !namespace && strings.HasPrefix(domain.String(rule, "namespace"), "!") && server:
			return false, nil
		}
	}
	return allowed, nil
}

// SourceNamespaceAllowed mirrors Argo CD v3.5.3 IsAppNamespacePermitted: the
// list is a plain OR and each entry is a glob without separators. Argo CD also
// evaluates "/regexp/" entries with regexp2 and treats a leading "!" literally,
// not as a negation. Those entries, invalid globs and non-string entries are
// reported as unsupported when no supported entry permits the namespace.
func SourceNamespaceAllowed(raw any, namespace string) (bool, error) {
	rules, _ := raw.([]any)
	if len(rules) > 1024 {
		return false, unsupportedPolicy("source namespace rules exceed the supported count")
	}
	var unsupported error
	for _, item := range rules {
		pattern, ok := item.(string)
		switch {
		case !ok:
			unsupported = unsupportedPolicy("source namespace rule must be a string")
		case strings.HasPrefix(pattern, "!"):
			unsupported = unsupportedPolicy("sourceNamespaces entries starting with ! are not negations in Argo CD and are not evaluated")
		case len(pattern) > 1 && strings.HasPrefix(pattern, "/") && strings.HasSuffix(pattern, "/"):
			unsupported = unsupportedPolicy("sourceNamespaces regular expressions are not supported")
		default:
			matched, err := Pattern(pattern, namespace)
			if err != nil {
				unsupported = err
				continue
			}
			if matched {
				return true, nil
			}
		}
	}
	return false, unsupported
}
