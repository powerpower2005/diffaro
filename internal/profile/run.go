package profile

import (
	"fmt"
	"regexp"
	"slices"
)

// Run holds operator defaults for command-line options, so a command needs
// only its revisions. Defaults apply to every doctor and diff run;
// --mode NAME applies Modes[NAME] over them. Explicit flags always win. The
// CLI checks option values with the same rules as the flags.
type Run struct {
	Defaults RunOptions            `yaml:"defaults,omitempty"`
	Modes    map[string]RunOptions `yaml:"modes,omitempty"`
}

// RunOptions are the profile forms of the diff and doctor options. Empty or
// absent fields leave the option to the next layer. Revisions, credentials
// and Helm selection are not here: revisions change per run, and
// credentials and Helm are instance settings.
type RunOptions struct {
	Format           string `yaml:"format,omitempty"`
	Output           string `yaml:"output,omitempty"`
	JSONOutput       string `yaml:"jsonOutput,omitempty"`
	Trace            string `yaml:"trace,omitempty"`
	DebugBundle      string `yaml:"debugBundle,omitempty"`
	DebugBundleLevel string `yaml:"debugBundleLevel,omitempty"`
	Recursive        *bool  `yaml:"recursive,omitempty"`
	Quiet            *bool  `yaml:"quiet,omitempty"`
	Verbose          *bool  `yaml:"verbose,omitempty"`
}

// MaxRunModes bounds run.modes.
const MaxRunModes = 16

var runModeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ModeNames lists the defined modes in sorted order.
func (r Run) ModeNames() []string {
	names := make([]string, 0, len(r.Modes))
	for name := range r.Modes {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (r Run) validate() error {
	if len(r.Modes) > MaxRunModes {
		return invalid(fmt.Sprintf("run.modes allows at most %d modes", MaxRunModes))
	}
	for _, name := range r.ModeNames() {
		if !runModeName.MatchString(name) {
			return invalid(fmt.Sprintf("run.modes names must be lowercase letters, digits and hyphens, starting with a letter, at most 32 characters: %q", name))
		}
	}
	return nil
}
