package preview

import (
	"context"
	"testing"

	"diffaro/internal/domain"
)

type versionReader struct {
	version string
	err     error
}

func (v versionReader) BackendVersion(context.Context) (string, error) { return v.version, v.err }

func TestVersionEvidenceDoesNotCertifyContext(t *testing.T) {
	for _, tc := range []struct {
		version, compatibility string
		err                    error
	}{
		{"v3.0.23", "version-in-tested-matrix", nil},
		{"v3.1.16", "version-in-tested-matrix", nil},
		{"v3.2.12", "version-in-tested-matrix", nil},
		{"v3.3.14", "version-in-tested-matrix", nil},
		{"v3.4.9", "version-in-tested-matrix", nil},
		{"v3.5.1+fixture", "version-in-tested-matrix", nil},
		{"v3.5.3+c9c369e", "version-in-tested-matrix", nil},
		{"v3.0.22", "version-not-in-tested-matrix", nil},
		{"v3.5.2", "version-not-in-tested-matrix", nil},
		{"v3.5.3-rc1", "version-not-in-tested-matrix", nil},
		{"v4.0.0", "version-not-in-tested-matrix", nil},
		{"", "version-unavailable", domain.Fail("access_denied", "permission", "denied", "allow version read", nil)},
	} {
		t.Run(tc.version+"/"+tc.compatibility, func(t *testing.T) {
			r := domain.NewReport()
			r.RenderBackends = []domain.BackendInfo{{Instance: "test", Verification: "live-unverified"}}
			InspectBackendVersions(context.Background(), &r, map[string]domain.BackendVersionReader{"test": versionReader{tc.version, tc.err}})
			if r.RenderBackends[0].Compatibility != tc.compatibility || r.RenderBackends[0].Verification != "live-unverified" {
				t.Fatal("version evidence overstated environment compatibility")
			}
			if tc.err != nil && (r.Status != "partial_failure" || len(r.Issues) != 1) {
				t.Fatal("version query failure hidden")
			}
		})
	}
}

func TestServerVersionDoesNotCertifyBundledHelm(t *testing.T) {
	r := domain.NewReport()
	r.RenderBackends = []domain.BackendInfo{{Instance: "test", Kind: "local-helm", Compatibility: "bounded-git-helm", Verification: "local-context-unverified"}}
	InspectBackendVersions(context.Background(), &r, map[string]domain.BackendVersionReader{"test": versionReader{"v3.5.1", nil}})
	if r.RenderBackends[0].Compatibility != "bounded-git-helm" || r.RenderBackends[0].Verification != "local-context-unverified" || r.RenderBackends[0].Version != "v3.5.1" {
		t.Fatal("server version certified another renderer")
	}
}
