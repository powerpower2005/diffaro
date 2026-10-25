package cli

import (
	"runtime/debug"
	"testing"
)

func TestBuildIdentity(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-local"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "aaaaaaaaaaaabbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, {Key: "vcs.modified", Value: "true"}}}
	for _, tc := range []struct {
		name, tag, commit, version, revision string
		info                                 *debug.BuildInfo
	}{
		{"no metadata", "", "", "dev", "unknown", nil},
		{"ordinary build", "", "", "v0.0.0-local (modified)", "aaaaaaaaaaaa", info},
		{"tagged release", "v0.1.0", "ccccccccccccdddddddddddddddddddddddddddd", "v0.1.0", "cccccccccccc", info},
		{"local release preview", "v0.1.0-rc.1", "ccccccccccccdddddddddddddddddddddddddddd-dirty", "v0.1.0-rc.1 (modified)", "cccccccccccc", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version, revision := buildIdentity(tc.tag, tc.commit, tc.info)
			if version != tc.version || revision != tc.revision {
				t.Fatalf("got %q / %q; want %q / %q", version, revision, tc.version, tc.revision)
			}
		})
	}
}
