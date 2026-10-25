package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

// initOptions holds the init flags. --connections selects the offline catalog
// path (initFromConnections); otherwise --endpoint connects to one instance
// (initFromEndpoint).
type initOptions struct {
	credentials       *credentialOptions
	endpoint          string
	connections       string
	localChildren     bool
	childNamespace    string
	namespaceProvided bool
	instance          string
	tokenEnv          string
	gitToken          string
	gitAPI            string
	prURL             string
	app               string
	set               string
	config            string
	limits            profile.Render
	single            bool
	repoAddress       string
	forward           bool
	forwardResource   string
	forwardPort       int
	grpcurl           string
	kubectl           string
	kubeconfig        string
	kubeContext       string
	argoNamespace     string
	plain             bool
	ca                string
	localHelm         bool
	helmBinary        string
	aliases           stringList
	mirrorHosts       stringList
	selection         string
	helmEnv           stringList
	helmEnvFrom       stringList
	// helmEnvironment is parsed from helmEnv and helmEnvFrom.
	helmEnvironment []profile.HelmEnvVar
}

func bindInitFlags(f *flag.FlagSet) *initOptions {
	o := &initOptions{credentials: bindCredentials(f), limits: profile.DefaultRender()}
	f.StringVar(&o.endpoint, "endpoint", "", "Argo CD HTTPS endpoint")
	f.StringVar(&o.connections, "connections", "", "trusted server catalog; create a repository-discovery profile offline with --pr")
	f.BoolVar(&o.localChildren, "local-children", false, "with --connections: explicitly route local in-cluster/default-service children to their parent's instance")
	f.StringVar(&o.childNamespace, "child-namespace", "argocd", "with --local-children: child Application CR namespace")
	f.StringVar(&o.instance, "instance", "default", "instance ID")
	f.StringVar(&o.tokenEnv, "token-env", "DIFFARO_ARGOCD_TOKEN", "Argo token environment variable")
	f.StringVar(&o.gitToken, "git-token-env", "DIFFARO_GITHUB_TOKEN", "Git token environment variable")
	f.StringVar(&o.gitAPI, "git-api", "", "explicit GitHub-compatible API URL")
	f.StringVar(&o.prURL, "pr", "", "PR URL for repository discovery without root names")
	f.StringVar(&o.app, "application", "", "namespace/name")
	f.StringVar(&o.set, "application-set", "", "namespace/name of current generated selection")
	f.StringVar(&o.config, "config", "diffaro.local.yaml", "new profile path")
	f.IntVar(&o.limits.MaxApplications, "max-applications", o.limits.MaxApplications, fmt.Sprintf("maximum readable inventory and graph Applications (1–%d); saved in the new profile", domain.MaxRenderApplications))
	f.Int64Var(&o.limits.MaxResponseBytes, "max-response-bytes", o.limits.MaxResponseBytes, "API/manifest byte limit (1024–67108864); saved in the new profile")
	f.StringVar(&o.limits.Timeout, "timeout", o.limits.Timeout, "per-stage render timeout (1s–10m); saved in the new profile")
	f.IntVar(&o.limits.MaxConcurrency, "max-concurrency", o.limits.MaxConcurrency, fmt.Sprintf("maximum concurrent Application renders (1–%d); saved in the new profile", domain.MaxRenderConcurrency))
	f.Int64Var(&o.limits.MaxSourceSnapshotBytes, "max-source-snapshot-bytes", 0, "local Helm per-instance source snapshot limit (maxResponseBytes–536870912; 0 keeps the 64 MiB default); saved in the new profile")
	f.BoolVar(&o.single, "single-instance", false, "explicitly route all recursive children to this instance")
	f.StringVar(&o.repoAddress, "repo-server", "", "repo-server host:port for automatic Kubernetes context collection")
	f.BoolVar(&o.forward, "repo-server-forward", false, "automatically forward the configured repo-server through Kubernetes")
	f.StringVar(&o.forwardResource, "repo-server-resource", "service/argocd-repo-server", "Kubernetes resource for automatic port forwarding")
	f.IntVar(&o.forwardPort, "repo-server-port", 8081, "remote repo-server port")
	f.StringVar(&o.grpcurl, "grpcurl", "", "grpcurl executable (defaults to PATH lookup)")
	f.StringVar(&o.kubectl, "kubectl", "", "kubectl executable (defaults to PATH lookup)")
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "explicit kubeconfig for optional read-only context collection")
	f.StringVar(&o.kubeContext, "kube-context", "", "explicit context in kubeconfig")
	f.StringVar(&o.argoNamespace, "argocd-namespace", "argocd", "Argo CD configuration namespace")
	f.BoolVar(&o.plain, "repo-server-plaintext", false, "allow loopback-only plaintext repo-server")
	f.StringVar(&o.ca, "repo-server-ca", "", "repo-server CA file")
	f.BoolVar(&o.localHelm, "local-helm", false, "save local Helm rendering; requires only GitHub and Argo CD read tokens")
	f.StringVar(&o.helmBinary, "helm-binary", "", "save a trusted Helm 3/4 executable path; selects local Helm")
	f.Var(&o.helmEnv, "helm-env", "with --helm-binary: NAME=VALUE passed to the external Helm executable, such as a service its template functions call (repeatable)")
	f.Var(&o.helmEnvFrom, "helm-env-from", "with --helm-binary: NAME=ENV passes NAME with the value of environment variable ENV at run time, for secrets (repeatable)")
	f.StringVar(&o.selection, "selection", "", "with --pr: changedPaths (default) skips candidates proven unaffected by the PR's changed files; all renders every candidate")
	f.Var(&o.aliases, "repository-alias", "with --pr: another HTTPS URL of the PR repository used by Argo CD sources, such as a mirror (repeatable)")
	f.Var(&o.mirrorHosts, "mirror-host", "HTTPS host of a read-through mirror of the Git host used by Argo CD sources, such as https://mirror.example.com (repeatable)")
	return o
}

func initialize(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("init", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	o := bindInitFlags(f)
	if proceed, code := parseOptions(f, args, stdout, stderr); !proceed {
		return code
	}
	var err error
	if o.helmBinary, o.localHelm, err = selectHelmBinary(o.helmBinary, o.localHelm); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return domain.ExitInvalidConfiguration
	}
	if o.helmEnvironment, err = parseHelmEnvironment(o.helmEnv, o.helmEnvFrom); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return domain.ExitInvalidConfiguration
	}
	if len(o.helmEnvironment) > 0 && o.helmBinary == "" {
		fmt.Fprintln(stderr, "init --helm-env and --helm-env-from require --helm-binary: only an external Helm executable receives environment variables")
		return domain.ExitInvalidConfiguration
	}
	f.Visit(func(option *flag.Flag) {
		if option.Name == "child-namespace" {
			o.namespaceProvided = true
		}
	})
	if err := o.limits.Validate(); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return domain.ExitInvalidConfiguration
	}
	if (len(o.aliases) > 0 || o.selection != "") && o.prURL == "" {
		fmt.Fprintln(stderr, "init --repository-alias and --selection require --pr")
		return domain.ExitInvalidConfiguration
	}
	if o.connections != "" {
		return initFromConnections(f, o, stdout, stderr)
	}
	return initFromEndpoint(ctx, f, o, stdout, stderr)
}

// parseHelmEnvironment reads --helm-env NAME=VALUE and --helm-env-from
// NAME=ENV in flag order; the profile validates names and values.
func parseHelmEnvironment(values, fromEnv []string) ([]profile.HelmEnvVar, error) {
	var vars []profile.HelmEnvVar
	for _, entry := range values {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || value == "" {
			return nil, fmt.Errorf("--helm-env must be NAME=VALUE")
		}
		vars = append(vars, profile.HelmEnvVar{Name: name, Value: value})
	}
	for _, entry := range fromEnv {
		name, env, ok := strings.Cut(entry, "=")
		if !ok || name == "" || env == "" {
			return nil, fmt.Errorf("--helm-env-from must be NAME=ENV")
		}
		vars = append(vars, profile.HelmEnvVar{Name: name, ValueFromEnv: env})
	}
	return vars, nil
}

// checkNewProfilePath enforces that init never overwrites an existing file.
func checkNewProfilePath(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("configuration already exists; init never overwrites files")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("cannot inspect configuration path")
	}
	return nil
}
