package profile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalHelmProfileBoundary(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(valid, "    tokenEnv: TEST_ARGO_TOKEN", "    tokenEnv: TEST_ARGO_TOKEN\n    localHelm: true", 1)))
	if err != nil || !c.Instances[0].LocalHelm {
		t.Fatal("local Helm requires unexpected connection inputs", err)
	}
	c.Instances[0].RepoServer = &RepoServer{Address: "127.0.0.1:8081", Plaintext: true, GRPCURL: "grpcurl", ContextFile: "context.json"}
	if c.Validate() == nil {
		t.Fatal("two competing render backends accepted")
	}
}

func TestExternalHelmProfileAndRelativePaths(t *testing.T) {
	data := strings.Replace(valid, "    tokenEnv: TEST_ARGO_TOKEN", "    tokenEnv: TEST_ARGO_TOKEN\n    localHelm: true\n    helmBinary: tools/helm", 1)
	c, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "profile.yaml")
	if err = os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(file)
	if err != nil || loaded.Instances[0].HelmBinary != filepath.Join(dir, "tools", "helm") {
		t.Fatal("Helm path must be scoped to profile", err)
	}
	c.Instances[0].LocalHelm = false
	if c.Validate() == nil {
		t.Fatal("executable must be explicitly selected as local renderer")
	}
}

func TestExternalHelmEnvironmentBoundary(t *testing.T) {
	data := strings.Replace(valid, "    tokenEnv: TEST_ARGO_TOKEN", "    tokenEnv: TEST_ARGO_TOKEN\n    localHelm: true\n    helmBinary: tools/helm\n    helmEnvironment:\n      - name: METADATA_HOST\n        value: https://metadata.example.invalid\n      - name: METADATA_TOKEN\n        valueFromEnv: TEST_METADATA_TOKEN", 1)
	c, err := Parse([]byte(data))
	if err != nil || len(c.Instances[0].HelmEnvironment) != 2 {
		t.Fatal("allow-listed Helm environment rejected", err)
	}
	env, secrets, err := c.Instances[0].ResolveHelmEnvironment(func(name string) string {
		if name == "TEST_METADATA_TOKEN" {
			return "fictional-metadata-token"
		}
		return ""
	})
	if err != nil || strings.Join(env, ",") != "METADATA_HOST=https://metadata.example.invalid,METADATA_TOKEN=fictional-metadata-token" || len(secrets) != 1 || secrets[0] != "fictional-metadata-token" {
		t.Fatal("environment values or secret registration lost", err, env, secrets)
	}
	if _, _, err = c.Instances[0].ResolveHelmEnvironment(func(string) string { return "" }); err == nil || strings.Contains(err.Error(), "fictional") {
		t.Fatal("unset valueFromEnv must be invalid configuration", err)
	}
	invalid := map[string]HelmEnvVar{
		"reserved name":        {Name: "KUBECONFIG", Value: "/tmp/config"},
		"reserved lower case":  {Name: "path", Value: "/usr/bin"},
		"Helm prefix":          {Name: "HELM_PLUGINS", Value: "/plugins"},
		"Diffaro prefix":       {Name: "DIFFARO_GITHUB_TOKEN", Value: "x"},
		"loader injection":     {Name: "LD_PRELOAD", Value: "/tmp/x.so"},
		"macOS loader":         {Name: "DYLD_INSERT_LIBRARIES", Value: "/tmp/x.dylib"},
		"invalid name":         {Name: "METADATA-HOST", Value: "x"},
		"no source":            {Name: "METADATA_HOST"},
		"both sources":         {Name: "METADATA_HOST", Value: "x", ValueFromEnv: "TEST_HOST"},
		"multi-line value":     {Name: "METADATA_HOST", Value: "x\ny"},
		"oversized value":      {Name: "METADATA_HOST", Value: strings.Repeat("x", MaxHelmEnvValueBytes+1)},
		"invalid source name":  {Name: "METADATA_HOST", ValueFromEnv: "TEST-HOST"},
		"duplicate (any case)": {Name: "metadata_host", Value: "x"},
	}
	for name, v := range invalid {
		t.Run(name, func(t *testing.T) {
			bad := c
			bad.Instances = []Instance{c.Instances[0]}
			bad.Instances[0].HelmEnvironment = append([]HelmEnvVar{c.Instances[0].HelmEnvironment[0]}, v)
			if bad.Validate() == nil {
				t.Fatal("unsafe Helm environment accepted")
			}
		})
	}
	tooMany := c
	tooMany.Instances = []Instance{c.Instances[0]}
	tooMany.Instances[0].HelmEnvironment = nil
	for i := 0; i <= MaxHelmEnvironment; i++ {
		tooMany.Instances[0].HelmEnvironment = append(tooMany.Instances[0].HelmEnvironment, HelmEnvVar{Name: "METADATA_" + string(rune('A'+i)), Value: "x"})
	}
	if tooMany.Validate() == nil {
		t.Fatal("unbounded Helm environment accepted")
	}
	withRepoServer := c
	withRepoServer.Instances = []Instance{c.Instances[0]}
	withRepoServer.Instances[0].LocalHelm, withRepoServer.Instances[0].HelmBinary = false, ""
	withRepoServer.Instances[0].RepoServer = &RepoServer{Address: "127.0.0.1:8081", Plaintext: true, GRPCURL: "grpcurl", ContextFile: "context.json"}
	if withRepoServer.Validate() == nil {
		t.Fatal("repo-server renders with its own environment; local Helm environment must be rejected")
	}
}

func TestRepoServerProfile(t *testing.T) {
	with := func(block string) string {
		return strings.Replace(valid, "    tokenEnv: TEST_ARGO_TOKEN", "    tokenEnv: TEST_ARGO_TOKEN\n    repoServer:\n"+block, 1)
	}
	for _, tt := range []struct {
		name, block string
		valid       bool
	}{
		{"TLS", "      address: repo-server.example.com:8081\n      grpcurl: tools/grpcurl\n      contextFile: context.json", true},
		{"loopback", "      address: 127.0.0.1:8081\n      plaintext: true\n      grpcurl: tools/grpcurl\n      contextFile: context.json", true},
		{"remote plaintext", "      address: repo-server.example.com:8081\n      plaintext: true\n      grpcurl: tools/grpcurl\n      contextFile: context.json", false},
		{"missing context", "      address: repo-server.example.com:8081\n      grpcurl: tools/grpcurl", false},
		{"URL instead of address", "      address: https://repo-server.example.com:8081\n      grpcurl: tools/grpcurl\n      contextFile: context.json", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(with(tt.block)))
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%t err=%v", tt.valid, err)
			}
		})
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "profile.yaml")
	if err := os.WriteFile(file, []byte(with("      address: repo-server.example.com:8081\n      grpcurl: tools/grpcurl\n      contextFile: context.json\n      caFile: ca.pem")), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	rpc := c.Instances[0].RepoServer
	if rpc.GRPCURL != filepath.Join(dir, "tools", "grpcurl") || rpc.ContextFile != filepath.Join(dir, "context.json") || rpc.CAFile != filepath.Join(dir, "ca.pem") {
		t.Fatal("backend paths must resolve relative to the profile")
	}
}

func TestManagedForwardProfile(t *testing.T) {
	for _, tc := range []struct {
		name, resource, address, ca string
		port                        int
		valid                       bool
	}{
		{"service", "service/argocd-repo-server", "", "", 8081, true},
		{"pod", "pod/repo-0", "", "", 8081, true},
		{"direct address conflict", "service/repo", "127.0.0.1:8081", "", 8081, false},
		{"CA conflict", "service/repo", "", "ca.pem", 8081, false},
		{"unsupported resource", "secret/repo", "", "", 8081, false},
		{"bad port", "service/repo", "", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(valid))
			if err != nil {
				t.Fatal(err)
			}
			c.Instances[0].RepoServer = &RepoServer{Address: tc.address, CAFile: tc.ca, GRPCURL: "grpcurl", PortForward: &PortForward{Resource: tc.resource, Port: tc.port}, Kubernetes: &KubernetesContext{Kubectl: "kubectl", Kubeconfig: "config", Context: "local", Namespace: "argocd"}}
			if err := c.Validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}

const valid = `version: v1alpha1
git:
  apiURL: https://api.github.com
  repository: example/charts
  repositoryURL: https://github.com/example/charts.git
  tokenEnv: TEST_GITHUB_TOKEN
instances:
  - id: test
    endpoint: https://argocd.example.com
    tokenEnv: TEST_ARGO_TOKEN
applications:
  - instance: test
    namespace: argocd
    name: api
`

func TestValidation(t *testing.T) {
	tests := []struct {
		name, input string
		valid       bool
	}{
		{"valid", valid, true},
		{"unknown field", valid + "typo: true\n", false},
		{"duplicate key", valid + "version: v1alpha1\n", false},
		{"extra document", valid + "---\nversion: v1alpha1\n", false},
		{"unknown instance", strings.Replace(valid, "instance: test", "instance: missing", 1), false},
		{"unsafe endpoint", strings.Replace(valid, "https://argocd.example.com", "http://argocd.example.com", 1), false},
		{"embedded credential", strings.Replace(valid, "https://argocd.example.com", "https://user:password@argocd.example.com", 1), false},
		{"unbounded concurrency", valid + "render:\n  maxConcurrency: 1000\n", false},
		{"invalid pointer", valid + "sensitivePaths: [/data/bad~2escape]\n", false},
		{"wrong repository URL", strings.Replace(valid, "github.com/example/charts.git", "github.com/example/other.git", 1), false},
		{"bounded depth", valid + "render:\n  maxDepth: 8\n", true},
		{"unbounded depth", valid + "render:\n  maxDepth: 65\n", false},
		{"source snapshot limit", valid + "render:\n  maxResponseBytes: 50331648\n  maxSourceSnapshotBytes: 134217728\n", true},
		{"source snapshot below response limit", valid + "render:\n  maxSourceSnapshotBytes: 1048576\n", false},
		{"unbounded source snapshot", valid + "render:\n  maxSourceSnapshotBytes: 536870913\n", false},
		{"negative source snapshot", valid + "render:\n  maxSourceSnapshotBytes: -1\n", false},
		{"unknown route instance", valid + "routing:\n  defaultInstance: missing\n", false},
		{"duplicate identity", valid + "  - instance: test\n    namespace: argocd\n    name: api\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse([]byte(tt.input))
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%t err=%v", tt.valid, err)
			}
			if err == nil && c.Render.MaxResponseBytes == 0 {
				t.Fatal("defaults were lost")
			}
		})
	}
}

func TestLargeDefaultsPreserveExplicitLegacyLimits(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil || c.Render.MaxApplications != 1000 || c.Render.MaxResponseBytes != 64<<20 || c.Render.MaxDocuments != 10000 {
		t.Fatal("large-use defaults were lost", err)
	}
	c, err = Parse([]byte(valid + "render:\n  maxApplications: 100\n  maxResponseBytes: 8388608\n  maxDocuments: 2000\n"))
	if err != nil || c.Render.MaxApplications != 100 || c.Render.MaxResponseBytes != 8<<20 || c.Render.MaxDocuments != 2000 {
		t.Fatal("explicit user limits were silently increased", err)
	}
}

func TestSourceSnapshotDefault(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil || c.Render.MaxSourceSnapshotBytes != 0 || c.Render.SourceSnapshotBytes() != 64<<20 {
		t.Fatal("omitted source snapshot limit changed the 64 MiB default", err)
	}
	c, err = Parse([]byte(valid + "render:\n  maxSourceSnapshotBytes: 134217728\n"))
	if err != nil || c.Render.SourceSnapshotBytes() != 128<<20 {
		t.Fatal("explicit source snapshot limit was not applied", err)
	}
}

func TestRepositoryMatching(t *testing.T) {
	if !SameRepository("https://github.com/example/charts.git", "https://github.com/example/charts") {
		t.Fatal(".git spelling must match")
	}
	for _, other := range []string{"https://github.example.com/example/charts", "https://github.com/example/chart", "https://attacker.example/example/charts"} {
		if SameRepository("https://github.com/example/charts", other) {
			t.Fatal("different repository matched")
		}
	}
}

func TestRepositoryAliases(t *testing.T) {
	withAliases := func(aliases ...string) string {
		block := "  repositoryAliases:\n"
		for _, alias := range aliases {
			block += "    - " + alias + "\n"
		}
		return strings.Replace(valid, "  tokenEnv: TEST_GITHUB_TOKEN\n", block+"  tokenEnv: TEST_GITHUB_TOKEN\n", 1)
	}
	for _, tc := range []struct {
		name    string
		aliases []string
		ok      bool
	}{
		{"mirror", []string{"https://mirror.example.com/example/charts"}, true},
		{"mirror with suffix", []string{"https://mirror.example.com/example/charts.git"}, true},
		{"other repository", []string{"https://mirror.example.com/example/other"}, false},
		{"plain HTTP", []string{"http://mirror.example.com/example/charts"}, false},
		{"embedded credential", []string{"https://user:secret@mirror.example.com/example/charts"}, false},
		{"same as repositoryURL", []string{"https://github.com/example/charts"}, false},
		{"duplicate", []string{"https://mirror.example.com/example/charts", "https://mirror.example.com/example/charts.git"}, false},
		{"too many", []string{"https://a.example.com/example/charts", "https://b.example.com/example/charts", "https://c.example.com/example/charts", "https://d.example.com/example/charts", "https://e.example.com/example/charts", "https://f.example.com/example/charts", "https://g.example.com/example/charts", "https://h.example.com/example/charts", "https://i.example.com/example/charts"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(withAliases(tc.aliases...))); (err == nil) != tc.ok {
				t.Fatalf("valid=%t err=%v", tc.ok, err)
			}
		})
	}
	c, err := Parse([]byte(withAliases("https://mirror.example.com/example/charts")))
	if err != nil {
		t.Fatal(err)
	}
	for source, want := range map[string]bool{
		"https://github.com/example/charts":             true,
		"https://mirror.example.com/example/charts.git": true,
		"git@mirror.example.com:example/charts.git":     true,
		"https://mirror.example.com/example/other":      false,
		"https://other.example.com/example/charts":      false,
	} {
		if c.Git.Matches(source) != want {
			t.Fatalf("Matches(%s) != %t", source, want)
		}
	}
	// Without an explicit alias another host never matches.
	c.Git.RepositoryAliases = nil
	if c.Git.Matches("https://mirror.example.com/example/charts") {
		t.Fatal("mirror host was inferred without configuration")
	}
}

func TestRepositoryDiscoveryProfile(t *testing.T) {
	base := strings.Split(valid, "applications:")[0]
	for _, tc := range []struct {
		block string
		ok    bool
	}{
		{"discovery:\n  mode: repository\n", true},
		{"discovery:\n  mode: repository\n  projects: [team]\n", true},
		{"discovery:\n  mode: guessed\n", false},
		{"discovery:\n  mode: repository\n  selection: changedPaths\n", true},
		{"discovery:\n  mode: repository\n  selection: all\n", true},
		{"discovery:\n  mode: repository\n  selection: prefix\n", false},
		{"discovery:\n  mode: repository\n  projects: [team, team]\n", false},
		{"discovery:\n  mode: repository\napplications:\n  - instance: test\n    namespace: argocd\n    name: api\n", false},
	} {
		_, err := Parse([]byte(base + tc.block))
		if (err == nil) != tc.ok {
			t.Fatalf("unexpected validation: %s: %v", tc.block, err)
		}
	}
}

func TestAutomaticRenderContextProfile(t *testing.T) {
	block := "    repoServer:\n      address: 127.0.0.1:8081\n      plaintext: true\n      grpcurl: tools/grpcurl\n      kubernetes:\n        kubectl: tools/kubectl\n        kubeconfig: cluster.yaml\n        context: local-test\n        namespace: argocd\n"
	input := strings.Replace(valid, "    tokenEnv: TEST_ARGO_TOKEN\n", "    tokenEnv: TEST_ARGO_TOKEN\n"+block, 1)
	if _, err := Parse([]byte(input)); err != nil {
		t.Fatal(err)
	}
	for _, broken := range []string{strings.Replace(input, "        context: local-test\n", "", 1), strings.Replace(input, "      kubernetes:", "      contextFile: context.json\n      kubernetes:", 1)} {
		if _, err := Parse([]byte(broken)); err == nil {
			t.Fatal("incomplete or ambiguous context accepted")
		}
	}
	file := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(file, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if c.Instances[0].RepoServer.Kubernetes.Kubeconfig != filepath.Join(filepath.Dir(file), "cluster.yaml") {
		t.Fatal("kubeconfig path must be profile-relative")
	}
}

func TestConfigAcceptsStoredTokensWithoutEnvironmentNames(t *testing.T) {
	data := strings.ReplaceAll(valid, "tokenEnv: TEST_GITHUB_TOKEN", "token: fictional-config-git")
	data = strings.ReplaceAll(data, "tokenEnv: TEST_ARGO_TOKEN", "token: fictional-config-argo")
	if _, err := Parse([]byte(data)); err != nil {
		t.Fatal("stored credentials should be valid", err)
	}
}

// TestValidationNamesFailingEntry checks that a validation failure names the
// list entry and field, keeps the general rule as its prefix and never echoes
// a URL, path or token value.
func TestValidationNamesFailingEntry(t *testing.T) {
	second := "  - id: prod\n    endpoint: %s\n    tokenEnv: TEST_ARGO_TOKEN\n"
	withSecond := func(endpoint string) string {
		return strings.Replace(valid, "applications:\n", strings.Replace(second, "%s", endpoint, 1)+"applications:\n", 1)
	}
	for _, tc := range []struct {
		name, input string
		want        []string
	}{
		{"instance endpoint", withSecond("http://fictional-argo.example.com"), []string{"instances must have unique IDs, valid endpoints, tokens and optional tokenEnv names: ", "instances[1] (id=prod): endpoint must be HTTPS"}},
		{"instance token", strings.Replace(valid, "    tokenEnv: TEST_ARGO_TOKEN", "    token: 'fictional-sensitive token'", 1), []string{"instances[0] (id=test): token must be"}},
		{"duplicate instance", strings.Replace(withSecond("https://argocd.example.com"), "id: prod", "id: test", 1), []string{"instances[1] (id=test): id repeats an earlier instance"}},
		{"unsafe instance id", strings.Replace(valid, "id: test", "id: 'fictional sensitive'", 1), []string{"instances[0]: id must be"}},
		{"application instance", strings.Replace(valid, "instance: test", "instance: missing", 1), []string{"applications must have unique IDs and reference a configured instance: applications[0] instance"}},
		{"git API credentials", strings.Replace(valid, "apiURL: https://api.github.com", "apiURL: https://fictional-user:fictional-pass@api.github.com", 1), []string{"git.apiURL must be HTTPS (HTTP allowed only for loopback) without credentials, query or fragment"}},
		{"git token", strings.Replace(valid, "tokenEnv: TEST_GITHUB_TOKEN", "tokenEnv: 'fictional-sensitive env'", 1), []string{"git.repositoryURL, token or tokenEnv is invalid: git.tokenEnv"}},
		{"sensitive path", valid + "sensitivePaths: [/data, fictional-sensitive]\n", []string{"sensitivePaths[1]"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.input))
			if err == nil {
				t.Fatal("invalid configuration accepted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("message %q does not contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "fictional") || strings.Contains(err.Error(), "example.com") {
				t.Fatalf("message echoed a configured value: %q", err)
			}
		})
	}
}

func TestLoadKeepsOpenFailureCause(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if !errors.Is(err, fs.ErrNotExist) || !strings.HasPrefix(err.Error(), "cannot open configuration file: ") {
		t.Fatalf("missing file not distinguishable: %v", err)
	}
	_, err = LoadConnections(filepath.Join(t.TempDir(), "missing.yaml"))
	if !errors.Is(err, fs.ErrNotExist) || !strings.HasPrefix(err.Error(), "cannot open connections file: ") {
		t.Fatalf("missing connections file not distinguishable: %v", err)
	}
}

func TestStoredTokenValidationDoesNotEchoInvalidSecrets(t *testing.T) {
	for _, token := range []string{"fictional-token with space", "fictional-token\r\n", "fictional-token\x00", "fictional-token-é", strings.Repeat("x", 65537)} {
		if ValidToken(token) {
			t.Fatal("unsafe token accepted")
		}
	}
	for _, token := range []string{"", "fictional-token==", "fictional.jwt-token_123"} {
		if !ValidToken(token) {
			t.Fatal("valid token rejected")
		}
	}
	data := strings.Replace(valid, "tokenEnv: TEST_GITHUB_TOKEN", "token: 'fictional-sensitive-invalid token'", 1)
	_, err := Parse([]byte(data))
	if err == nil || strings.Contains(err.Error(), "fictional-") {
		t.Fatal("invalid stored token accepted or echoed")
	}
}

func TestMirrorHosts(t *testing.T) {
	withMirrors := func(hosts ...string) string {
		block := "  mirrorHosts:\n"
		for _, host := range hosts {
			block += "    - " + host + "\n"
		}
		return strings.Replace(valid, "  tokenEnv: TEST_GITHUB_TOKEN\n", block+"  tokenEnv: TEST_GITHUB_TOKEN\n", 1)
	}
	for _, tc := range []struct {
		name  string
		hosts []string
		ok    bool
	}{
		{"mirror host", []string{"https://mirror.example.com"}, true},
		{"trailing slash", []string{"https://mirror.example.com/"}, true},
		{"path", []string{"https://mirror.example.com/example"}, false},
		{"plain HTTP", []string{"http://mirror.example.com"}, false},
		{"embedded credential", []string{"https://user:secret@mirror.example.com"}, false},
		{"repository host itself", []string{"https://github.com"}, false},
		{"duplicate", []string{"https://mirror.example.com", "https://MIRROR.example.com"}, false},
		{"too many", []string{"https://a.example.com", "https://b.example.com", "https://c.example.com", "https://d.example.com", "https://e.example.com", "https://f.example.com", "https://g.example.com", "https://h.example.com", "https://i.example.com"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(withMirrors(tc.hosts...))); (err == nil) != tc.ok {
				t.Fatalf("valid=%t err=%v", tc.ok, err)
			}
		})
	}
	g := Git{RepositoryURL: "https://github.example.com/example/charts", MirrorHosts: []string{"https://mirror.example.com"}}
	if !g.Matches("https://mirror.example.com/example/charts.git") || g.Matches("https://mirror.example.com/example/other") || g.Matches("https://other.example.com/example/charts") {
		t.Fatal("mirror host must match only the selected repository's path")
	}
	for source, want := range map[string]string{
		"https://mirror.example.com/example/other.git": "https://github.example.com/example/other.git",
		"https://MIRROR.example.com/example/other":     "https://github.example.com/example/other",
		"https://other.example.com/example/other":      "https://other.example.com/example/other",
		"http://mirror.example.com/example/other":      "http://mirror.example.com/example/other",
		"https://u:p@mirror.example.com/example/other": "https://u:p@mirror.example.com/example/other",
	} {
		if got := Unmirror(source, g.RepositoryURL, g.MirrorHosts); got != want {
			t.Errorf("Unmirror(%q) = %q, want %q", source, got, want)
		}
	}
}

func TestPreexistingFailureExitOptIn(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil || c.Failures.ExcludePreexistingFromExit {
		t.Fatal("preexisting failures must count toward the exit code by default", err)
	}
	c, err = Parse([]byte(valid + "failures:\n  excludePreexistingFromExit: true\n"))
	if err != nil || !c.Failures.ExcludePreexistingFromExit {
		t.Fatal("explicit opt-in was not read", err)
	}
	if _, err = Parse([]byte(valid + "failures:\n  excludeAllFailures: true\n")); err == nil {
		t.Fatal("unknown failures setting accepted")
	}
}

func TestSamePathOtherHost(t *testing.T) {
	const repo = "https://github.example.com/team/charts"
	for source, want := range map[string]string{
		"https://mirror.example.com/team/charts":      "https://mirror.example.com/team/charts",
		"https://mirror.example.com/Team/Charts.git":  "https://mirror.example.com/Team/Charts",
		"https://github.example.com/team/charts.git":  "", // the repository itself
		"https://mirror.example.com/team/other":       "",
		"git@mirror.example.com:team/charts.git":      "", // a mirror host must be HTTPS
		"https://mirror.example.com:8443/team/charts": "",
		"https://user@mirror.example.com/team/charts": "",
	} {
		got, ok := SamePathOtherHost(source, repo)
		if got != want || ok != (want != "") {
			t.Errorf("%s: got %q %t, want %q", source, got, ok, want)
		}
	}
}

func TestLocalRepositories(t *testing.T) {
	with := func(block string) string {
		return strings.Replace(valid, "  tokenEnv: TEST_GITHUB_TOKEN\n", "  tokenEnv: TEST_GITHUB_TOKEN\n"+block, 1)
	}
	for _, tc := range []struct {
		name, block string
		ok          bool
	}{
		{"one repository", "  localRepositories:\n    - name: example/charts\n      path: '" + t.TempDir() + "'\n", true},
		{"relative path", "  localRepositories:\n    - name: example/charts\n      path: cache/charts.git\n", false},
		{"unclean path", "  localRepositories:\n    - name: example/charts\n      path: /cache/../charts.git\n", false},
		{"not owner/repository", "  localRepositories:\n    - name: charts\n      path: /cache/charts.git\n", false},
		{"repeated name", "  localRepositories:\n    - name: example/charts\n      path: /cache/a\n    - name: Example/Charts\n      path: /cache/b\n", false},
		{"relative git executable", "  gitExecutable: bin/git\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(with(tc.block)))
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%t", err, tc.ok)
			}
		})
	}
}
