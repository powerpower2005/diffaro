package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"diffaro/internal/profile"
)

// defaultConfig and initConfig are the profiles read when --config is not
// given: the shared profile first, then the one init writes by default.
const defaultConfig, initConfig = "diffaro.yaml", "diffaro.local.yaml"

// resolveConfigPath picks the profile to read: --config when given,
// otherwise diffaro.yaml, or diffaro.local.yaml when only that exists.
func resolveConfigPath(o *commandOptions) {
	if slices.Contains(o.flags, "config") {
		return
	}
	if _, err := os.Stat(defaultConfig); !errors.Is(err, fs.ErrNotExist) {
		return
	}
	if _, err := os.Stat(initConfig); err == nil {
		o.configPath = initConfig
	}
}

// applyRunOptions fills the options not given on the command line from the
// profile: run.defaults, then run.modes[--mode] over them.
// Options the command does not support are left alone, so one profile
// serves diff and doctor.
func applyRunOptions(o *commandOptions, run profile.Run) error {
	layers := []profile.RunOptions{run.Defaults}
	if o.mode != "" {
		mode, ok := run.Modes[o.mode]
		if !ok {
			defined := "none"
			if names := run.ModeNames(); len(names) > 0 {
				defined = strings.Join(names, ", ")
			}
			return fmt.Errorf("mode %q is not defined in the profile's run.modes (defined: %s)", o.mode, defined)
		}
		layers = append(layers, mode)
	}
	given := map[string]bool{}
	for _, name := range o.flags {
		given[name] = true
	}
	diff := o.command == "diff"
	for _, layer := range layers {
		setString(&o.format, layer.Format, given["format"] || (!diff && layer.Format == "pr-markdown"))
		setString(&o.output, layer.Output, given["output"])
		setString(&o.jsonOutput, layer.JSONOutput, given["json-output"] || !diff)
		setString(&o.trace, layer.Trace, given["trace"] || !diff)
		setString(&o.debugBundle, layer.DebugBundle, given["debug-bundle"] || !diff)
		setString(&o.debugBundleLevel, layer.DebugBundleLevel, given["debug-bundle-level"] || !diff)
		setBool(&o.recursive, layer.Recursive, given["recursive"] || !o.rendersTargets())
		setBool(&o.quiet, layer.Quiet, given["quiet"])
		setBool(&o.verbose, layer.Verbose, given["verbose"])
	}
	// A profile JSON output with JSON primary output would be rejected;
	// the primary report already is the JSON.
	if o.format == "json" && !given["json-output"] {
		o.jsonOutput = ""
	}
	return nil
}

func setString(option *string, value string, keep bool) {
	if value != "" && !keep {
		*option = value
	}
}

func setBool(option *bool, value *bool, keep bool) {
	if value != nil && !keep {
		*option = *value
	}
}

// checkRunProfile validates run.defaults and every mode as diff options, so
// validate reports a run setting that diff would reject.
func checkRunProfile(run profile.Run) error {
	for _, mode := range append([]string{""}, run.ModeNames()...) {
		o := &commandOptions{command: "diff", format: "markdown", debugBundleLevel: "public", mode: mode}
		if err := applyRunOptions(o, run); err != nil {
			return err
		}
		if err := o.validate(); err != nil {
			scope := "run.defaults"
			if mode != "" {
				scope = "run.modes." + mode
			}
			return fmt.Errorf("invalid configuration: %s: %w", scope, err)
		}
	}
	return nil
}

// describeRun lists, for validate, the diff options that run.defaults and
// each mode produce, so an operator sees what a short command will do.
func describeRun(run profile.Run) []string {
	if run.Defaults == (profile.RunOptions{}) && len(run.Modes) == 0 {
		return nil
	}
	var lines []string
	for _, mode := range append([]string{""}, run.ModeNames()...) {
		o := &commandOptions{command: "diff", format: "markdown", debugBundleLevel: "public", mode: mode}
		if applyRunOptions(o, run) != nil {
			continue
		}
		label, command := "Run defaults", "diffaro diff"
		if mode != "" {
			label, command = "Run mode "+mode, "diffaro diff --mode "+mode
		}
		lines = append(lines, fmt.Sprintf("%s (%s): %s", label, command, strings.Join(effectiveOptions(o), ", ")))
	}
	return lines
}

func effectiveOptions(o *commandOptions) []string {
	options := []string{"format " + o.format}
	for _, path := range []struct{ name, value string }{{"output", o.output}, {"json-output", o.jsonOutput}, {"trace", o.trace}} {
		if path.value != "" {
			options = append(options, path.name+" "+path.value)
		}
	}
	if o.output == "" {
		options = append(options, "report to stdout")
	}
	if o.debugBundle != "" {
		options = append(options, "debug-bundle "+o.debugBundle+" ("+o.debugBundleLevel+")")
	}
	for _, flag := range []struct {
		name string
		on   bool
	}{{"recursive", o.recursive}, {"quiet", o.quiet}, {"verbose", o.verbose}} {
		if flag.on {
			options = append(options, flag.name)
		}
	}
	return options
}
