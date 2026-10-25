// Package profile loads and validates the trusted YAML profile (Config,
// Load, Validate) and connection catalogs, and routes child Applications to
// Argo CD instances (Routing.Route). A token is stored in the profile
// (git.token, instances[].token) or named by tokenEnv, which the CLI reads
// when no token is stored. Repository identity (SameRepository,
// Unmirror, Git.Matches) is in repository.go.
package profile

import (
	"time"

	"diffaro/internal/domain"
)

const MaxConfigBytes = 1 << 20

type Instance struct {
	Token      string `yaml:"token,omitempty" json:"-"`
	LocalHelm  bool   `yaml:"localHelm,omitempty"`
	HelmBinary string `yaml:"helmBinary,omitempty"`
	// HelmEnvironment is the operator allow-list passed to an external Helm
	// executable. Everything else stays excluded from its environment.
	HelmEnvironment []HelmEnvVar `yaml:"helmEnvironment,omitempty"`
	RepoServer      *RepoServer  `yaml:"repoServer"`
	ID              string       `yaml:"id"`
	Endpoint        string       `yaml:"endpoint"`
	TokenEnv        string       `yaml:"tokenEnv,omitempty"`
}

type RepoServer struct {
	PortForward *PortForward       `yaml:"portForward,omitempty"`
	Kubernetes  *KubernetesContext `yaml:"kubernetes,omitempty"`
	Address     string             `yaml:"address"`
	GRPCURL     string             `yaml:"grpcurl"`
	ContextFile string             `yaml:"contextFile"`
	CAFile      string             `yaml:"caFile"`
	Plaintext   bool               `yaml:"plaintext"`
}

type PortForward struct {
	Resource string `yaml:"resource"`
	Port     int    `yaml:"port"`
}

type KubernetesContext struct {
	Kubectl    string `yaml:"kubectl"`
	Kubeconfig string `yaml:"kubeconfig"`
	Context    string `yaml:"context"`
	Namespace  string `yaml:"namespace"`
}
type Git struct {
	Token         string `yaml:"token,omitempty" json:"-"`
	APIURL        string `yaml:"apiURL"`
	Repository    string `yaml:"repository"`
	RepositoryURL string `yaml:"repositoryURL"`
	// RepositoryAliases are other URLs of the same repository that Argo CD
	// sources may use, such as a read-through mirror. Credentials and API
	// calls always use apiURL; aliases only widen source identity matching.
	RepositoryAliases []string `yaml:"repositoryAliases,omitempty"`
	// MirrorHosts are read-through mirrors of the repositoryURL host, such as
	// https://mirror.example.com: a source https://MIRROR/OWNER/REPO names the
	// same OWNER/REPO. Mirrored sources are read through apiURL; credentials
	// are never sent to a mirror.
	MirrorHosts []string `yaml:"mirrorHosts,omitempty"`
	TokenEnv    string   `yaml:"tokenEnv,omitempty"`
	// LocalRepositories serve the immutable content of the named
	// repositories, read by commit SHA, from local Git repositories that the
	// operator keeps fetched; refs and pull request data still come from
	// apiURL.
	LocalRepositories []LocalRepository `yaml:"localRepositories,omitempty"`
	// GitExecutable is the git command that reads LocalRepositories; empty
	// looks git up on PATH.
	GitExecutable string `yaml:"gitExecutable,omitempty"`
}

// LocalRepository maps an owner/repository on the configured Git host to a
// local Git repository directory.
type LocalRepository struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

// MaxLocalRepositories bounds git.localRepositories.
const MaxLocalRepositories = 32

type Render struct {
	MaxDepth         int    `yaml:"maxDepth"`
	Timeout          string `yaml:"timeout"`
	MaxConcurrency   int    `yaml:"maxConcurrency"`
	MaxApplications  int    `yaml:"maxApplications"`
	MaxResponseBytes int64  `yaml:"maxResponseBytes"`
	MaxDocuments     int    `yaml:"maxDocuments"`
	// MaxSourceSnapshotBytes bounds source files and chart dependencies that
	// local Helm keeps in memory per instance. Zero selects the 64 MiB default.
	MaxSourceSnapshotBytes int64 `yaml:"maxSourceSnapshotBytes,omitempty"`
}

const DefaultSourceSnapshotBytes, MaxSourceSnapshotBytes = int64(64 << 20), int64(domain.MaxRenderSourceSnapshotBytes)

// SourceSnapshotBytes returns the effective local Helm source snapshot limit.
func (r Render) SourceSnapshotBytes() int64 {
	if r.MaxSourceSnapshotBytes == 0 {
		return DefaultSourceSnapshotBytes
	}
	return r.MaxSourceSnapshotBytes
}

func DefaultRender() Render {
	return Render{Timeout: "30s", MaxDepth: 16, MaxConcurrency: 4, MaxApplications: 1000, MaxResponseBytes: 64 << 20, MaxDocuments: 10000}
}

type Config struct {
	Policies        []domain.PolicyRule    `yaml:"policies,omitempty"`
	Discovery       *Discovery             `yaml:"discovery,omitempty"`
	Routing         Routing                `yaml:"routing"`
	Version         string                 `yaml:"version"`
	Git             Git                    `yaml:"git"`
	Instances       []Instance             `yaml:"instances"`
	Applications    []domain.ApplicationID `yaml:"applications"`
	ApplicationSets []domain.ApplicationID `yaml:"applicationSets,omitempty"`
	Render          Render                 `yaml:"render"`
	SensitivePaths  []string               `yaml:"sensitivePaths"`
	Failures        Failures               `yaml:"failures,omitempty"`
	// Run holds defaults and named modes for command-line options.
	Run Run `yaml:"run,omitempty"`
}

// Failures adjusts how failures decide the exit code; reports keep every
// failure.
type Failures struct {
	// ExcludePreexistingFromExit leaves Applications already failing before
	// the PR out of the exit code. Off by default.
	ExcludePreexistingFromExit bool `yaml:"excludePreexistingFromExit,omitempty"`
}

type Discovery struct {
	Mode     string   `yaml:"mode"`
	Projects []string `yaml:"projects,omitempty"`
	// Selection "changedPaths" skips candidates proven unaffected by the PR's
	// changed files. Empty or "all" renders every candidate.
	Selection string `yaml:"selection,omitempty"`
}

const SelectionChangedPaths = "changedPaths"

func (c Config) Timeout() time.Duration { t, _ := time.ParseDuration(c.Render.Timeout); return t }
