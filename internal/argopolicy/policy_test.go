package argopolicy

import "testing"

const repository = "https://github.com/example/charts.git"

func TestProjectSourceGlobAndDeny(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rules      []any
		repo       string
		allow, bad bool
	}{
		{"default wildcard", []any{"*"}, repository, true, false},
		{"organization", []any{"https://github.com/example/*"}, repository, true, false},
		{"nested separator", []any{"https://github.com/*"}, repository, false, false},
		{"recursive wildcard", []any{"https://github.com/**"}, repository, true, false},
		{"explicit deny", []any{"*", "!https://github.com/example/*"}, repository, false, false},
		{"deny first", []any{"!https://github.com/example/*", "*"}, repository, false, false},
		{"unrelated deny", []any{"*", "!https://denied.example/**"}, repository, true, false},
		{"normalized", []any{"HTTPS://GITHUB.COM/EXAMPLE/CHARTS"}, repository, true, false},
		{"empty", []any{}, repository, false, false},
		{"invalid negative wildcard", []any{"!*"}, repository, false, true},
		{"invalid pattern", []any{"https://github.com/example/["}, repository, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SourceAllowed(tc.rules, tc.repo)
			if (err != nil) != tc.bad || (err == nil && got != tc.allow) {
				t.Fatalf("allow=%t err=%v", got, err)
			}
		})
	}
}

func TestProjectDestinationGlobAndDeny(t *testing.T) {
	dest := map[string]any{"name": "production-east", "server": "https://cluster.example", "namespace": "team-api"}
	rule := func(name, server, namespace string) map[string]any {
		return map[string]any{"name": name, "server": server, "namespace": namespace}
	}
	allow := rule("", "*", "*")
	for _, tc := range []struct {
		name    string
		rules   []any
		allowed bool
	}{
		{"default", []any{allow}, true},
		{"name wildcard", []any{rule("production-*", "", "team-*")}, true},
		{"server wildcard", []any{rule("", "https://*.example", "team-*")}, true},
		{"wrong namespace", []any{rule("", "*", "other-*")}, false},
		{"server deny", []any{allow, rule("", "!https://cluster.example", "*")}, false},
		{"namespace deny", []any{allow, rule("", "*", "!team-*")}, false},
		{"name deny", []any{allow, rule("!production-*", "", "*")}, false},
		{"unrelated server deny", []any{allow, rule("", "!https://other.example", "*")}, true},
		{"deny first", []any{rule("", "!https://cluster.example", "*"), allow}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DestinationAllowed(tc.rules, dest)
			if err != nil || got != tc.allowed {
				t.Fatalf("allow=%t err=%v", got, err)
			}
		})
	}
}
