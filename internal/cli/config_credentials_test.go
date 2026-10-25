package cli

import (
	"bytes"
	"context"
	"diffaro/internal/domain"
	"diffaro/internal/profile"
	"diffaro/internal/security"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConfigTokensOverrideEnvironmentAndFlagsOverrideConfig(t *testing.T) {
	for _, flags := range []bool{false, true} {
		t.Run(map[bool]string{false: "config", true: "flags"}[flags], func(t *testing.T) {
			git := gitServer(t)
			argo, _ := argoServer(t, "fictional-argo-0", backendOptions{})
			cfg := configFile(t, git.URL, argo.URL)
			gitToken, argoToken := "fictional-github-token", "fictional-argo-0"
			if flags {
				gitToken, argoToken = "fictional-old-git", "fictional-old-argo"
			}
			editProfile(t, cfg, "tokenEnv: DIFFARO_TEST_GITHUB", "tokenEnv: DIFFARO_TEST_GITHUB\n  token: "+gitToken)
			editProfile(t, cfg, "tokenEnv: DIFFARO_TEST_ARGO_0", "tokenEnv: DIFFARO_TEST_ARGO_0\n    token: "+argoToken)
			raw, err := os.ReadFile(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("DIFFARO_TEST_GITHUB", "fictional-wrong-env-git")
			t.Setenv("DIFFARO_TEST_ARGO_0", "fictional-wrong-env-argo")
			args := []string{"diff", "--config", cfg, "--base", "main", "--head", "feature", "--format", "json", "--quiet"}
			if flags {
				args = append(args, "--git-token", "fictional-github-token", "--argocd-token", "fictional-argo-0")
			}
			var out, diagnostic bytes.Buffer
			if code := runDiffaro(t, args, &out, &diagnostic); code != 1 {
				t.Fatalf("credential selection failed: %d %s", code, diagnostic.String())
			}
			assertNoSecrets(t, "diff output", out.String()+diagnostic.String(), "fictional-")
			after, _ := os.ReadFile(cfg)
			if !bytes.Equal(raw, after) {
				t.Fatal("execution flags rewrote config")
			}
		})
	}
}

func TestCredentialFlagsValidateAndIsolateInstances(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantError bool
	}{
		{"per instance", []string{"--instance-token", "dev=fictional-new-dev=="}, false},
		{"empty git", []string{"--git-token="}, true},
		{"empty argo", []string{"--argocd-token="}, true},
		{"ambiguous argo", []string{"--argocd-token", "fictional-value"}, true},
		{"unknown", []string{"--instance-token", "unknown=fictional-value"}, true},
		{"duplicate", []string{"--instance-token", "dev=fictional-one", "--instance-token", "dev=fictional-two"}, true},
		{"missing separator", []string{"--instance-token", "fictional-value"}, true},
		{"blank", []string{"--instance-token", "dev="}, true},
		{"header injection", []string{"--instance-token", "dev=fictional-value\r\n"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DIFFARO_TEST_CREDENTIAL_ENV", "fictional-env")
			c := profile.Config{Git: profile.Git{Token: "fictional-config-git", TokenEnv: "DIFFARO_TEST_CREDENTIAL_ENV"}, Instances: []profile.Instance{{ID: "dev", Token: "fictional-config-dev", TokenEnv: "DIFFARO_TEST_CREDENTIAL_ENV"}, {ID: "prod", Token: "fictional-config-prod"}}}
			f := flag.NewFlagSet("test", flag.ContinueOnError)
			o := bindCredentials(f)
			if err := f.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			secrets, err := o.resolve(f, &c)
			if (err != nil) != tc.wantError {
				t.Fatal("unexpected credential validation result", err)
			}
			if err != nil {
				assertNoSecrets(t, "credential validation error", err.Error(), "fictional-")
				return
			}
			if c.Instances[0].Token != "fictional-new-dev==" || c.Instances[1].Token != "fictional-config-prod" || c.Git.Token != "fictional-config-git" {
				t.Fatal("credentials crossed instance boundaries")
			}
			redactor := security.Redactor{Secrets: secrets}
			text := redactor.Text("fictional-new-dev== fictional-config-dev fictional-config-prod fictional-config-git fictional-env")
			assertNoSecrets(t, "redacted overridden credentials", text, "fictional-")
		})
	}
}

func TestInitSavesFlagCredentialsAndReusesWithoutEnvironment(t *testing.T) {
	git := gitServer(t)
	argo, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	t.Setenv("DIFFARO_GITHUB_TOKEN", "")
	t.Setenv("DIFFARO_ARGOCD_TOKEN", "")
	path := filepath.Join(t.TempDir(), "private.yaml")
	var out, diagnostic bytes.Buffer
	args := []string{"init", "--endpoint", argo.URL, "--application", "argocd/api", "--git-api", git.URL, "--config", path, "--git-token", "fictional-github-token", "--instance-token", "default=fictional-argo-0"}
	if code := runDiffaro(t, args, &out, &diagnostic); code != 0 {
		t.Fatalf("init: %d %s", code, diagnostic.String())
	}
	c, err := profile.Load(path)
	if err != nil || c.Git.Token != "fictional-github-token" || c.Instances[0].Token != "fictional-argo-0" {
		t.Fatal("flag credentials not saved", err)
	}
	assertPrivateFile(t, path)
	if runtime.GOOS == "windows" && (!strings.Contains(out.String(), "directory ACLs") || strings.Contains(out.String(), "0600")) {
		t.Fatal("Windows init must explain directory access rather than promise POSIX permissions")
	}
	assertNoSecrets(t, "init output", out.String()+diagnostic.String(), "fictional-")
	out.Reset()
	diagnostic.Reset()
	if code := runDiffaro(t, []string{"diff", "--config", path, "--base", "main", "--head", "feature", "--format", "json", "--quiet"}, &out, &diagnostic); code != 1 {
		t.Fatalf("saved credentials failed: %d %s", code, diagnostic.String())
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, "profile JSON", string(encoded), "fictional-")
}

func TestValidateDoesNotInspectCredentialEnvironment(t *testing.T) {
	git := gitServer(t)
	argo, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	path := configFile(t, git.URL, argo.URL)
	t.Setenv("DIFFARO_TEST_GITHUB", "fictional-invalid\n")
	t.Setenv("DIFFARO_TEST_ARGO_0", "fictional-invalid\n")
	var out, diagnostic bytes.Buffer
	if code := Run(context.Background(), []string{"validate", "--config", path}, &out, &diagnostic); code != 0 {
		t.Fatal("offline validate depends on credential environment", code, diagnostic.String())
	}
}

func TestInstanceTokenOverridesReachOnlyTheirOwnBackend(t *testing.T) {
	git := gitServer(t)
	dev, _ := argoServer(t, "fictional-argo-0", backendOptions{})
	prod, _ := argoServer(t, "fictional-argo-1", backendOptions{})
	path := configFile(t, git.URL, dev.URL, prod.URL)
	t.Setenv("DIFFARO_TEST_ARGO_0", "fictional-wrong-dev")
	t.Setenv("DIFFARO_TEST_ARGO_1", "fictional-wrong-prod")
	var out, diagnostic bytes.Buffer
	args := []string{"diff", "--config", path, "--base", "main", "--head", "feature", "--format", "json", "--quiet", "--instance-token", "instance-0=fictional-argo-0", "--instance-token", "instance-1=fictional-argo-1"}
	if code := runDiffaro(t, args, &out, &diagnostic); code != 1 {
		t.Fatalf("multi-instance overrides: %d %s", code, diagnostic.String())
	}
	assertNoSecrets(t, "diff output", out.String()+diagnostic.String(), "fictional-")
	var report domain.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || len(report.Applications) != 2 {
		t.Fatal("independent results lost", err)
	}
}
