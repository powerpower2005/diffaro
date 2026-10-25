package cli

import (
	"errors"
	"flag"
	"fmt"
	"slices"

	"diffaro/internal/debugbundle"
	"diffaro/internal/reporter"
)

// commandOptions holds the flags shared by validate, doctor and diff.
type commandOptions struct {
	command     string
	credentials *credentialOptions // nil for validate, which never reads credentials
	configPath  string
	base, head  string
	prInput     string
	format      string
	formatSet   bool // --format was given; validate rejects it
	output      string
	jsonOutput  string
	trace       string
	// debugBundle and debugBundleLevel select the opt-in debug bundle
	//; flags names the options given, for the bundle.
	debugBundle      string
	debugBundleLevel string
	// mode names the profile run mode applied over run.defaults.
	mode        string
	flags       []string
	recursive   bool
	renderCheck bool
	quiet       bool
	verbose     bool
	localHelm   bool
	helmBinary  string
}

func bindCommandFlags(f *flag.FlagSet, command string) *commandOptions {
	o := &commandOptions{command: command}
	if command != "validate" {
		o.credentials = bindCredentials(f)
	}
	f.StringVar(&o.configPath, "config", defaultConfig, "profile path; without it, "+defaultConfig+" or else "+initConfig)
	f.StringVar(&o.mode, "mode", "", "diff/doctor: apply the profile's run.modes NAME over run.defaults")
	f.StringVar(&o.base, "base", "", "base ref")
	f.StringVar(&o.head, "head", "", "head ref")
	f.StringVar(&o.prInput, "pr", "", "PR number or URL")
	f.StringVar(&o.format, "format", "markdown", "report format: markdown, pr-markdown, json or terminal")
	f.StringVar(&o.output, "output", "", "output file")
	f.StringVar(&o.jsonOutput, "json-output", "", "write the same diff report as JSON")
	f.StringVar(&o.trace, "trace", "", "diff: write a diagnostic phase trace JSON; not a report")
	f.StringVar(&o.debugBundle, "debug-bundle", "", "diff: write a debug bundle for a bug report; public level pseudonymizes names")
	f.StringVar(&o.debugBundleLevel, "debug-bundle-level", debugbundle.LevelPublic, "debug bundle level: public (safe for public issues) or private (names and the redacted report)")
	f.BoolVar(&o.recursive, "recursive", false, "recursively discover target Applications")
	f.BoolVar(&o.renderCheck, "render", false, "doctor: verify actual target rendering")
	f.BoolVar(&o.quiet, "quiet", false, "hide progress and completion messages; errors and skips remain visible")
	f.BoolVar(&o.verbose, "verbose", false, "diff/doctor: print detailed safe failure and skip context to stderr")
	f.BoolVar(&o.localHelm, "local-helm", false, "render Git Helm locally using the bundled Helm engine and GitHub/Argo CD read tokens")
	f.StringVar(&o.helmBinary, "helm-binary", "", "trusted Helm 3/4 executable path; selects local Helm and overrides all instance helmBinary paths")
	return o
}

// validate rejects flag combinations that the selected command does not support.
func (o *commandOptions) validate() error {
	command := o.command
	switch {
	case !slices.Contains(reporter.Formats, o.format):
		return errors.New("format must be " + orList(reporter.Formats))
	case o.format == "pr-markdown" && command != "diff":
		return fmt.Errorf("pr-markdown is supported only by diff")
	case o.renderCheck && command != "doctor":
		return fmt.Errorf("render is supported only by doctor")
	case command == "doctor" && !o.renderCheck && (o.base != "" || o.head != "" || o.prInput != ""):
		return fmt.Errorf("doctor revision arguments require --render")
	case o.recursive && command != "diff" && !(command == "doctor" && o.renderCheck):
		return fmt.Errorf("recursive requires diff or doctor --render")
	case o.mode != "" && command == "validate":
		return fmt.Errorf("mode is supported only by diff and doctor; validate checks every mode")
	case o.verbose && command == "validate":
		return fmt.Errorf("verbose is supported only by diff and doctor")
	case (o.formatSet || o.output != "") && command == "validate":
		return fmt.Errorf("format and output are supported only by diff and doctor")
	case (o.base != "" || o.head != "" || o.prInput != "") && command == "validate":
		return fmt.Errorf("revision arguments are supported only by diff and doctor --render")
	}
	// Equal paths are rejected here, before the profile or any credential is
	// read; prepareOutputs repeats the check on the file system (symlinked
	// directories, hard links) once the run is about to start.
	if o.trace != "" {
		if command != "diff" {
			return fmt.Errorf("trace is supported only by diff")
		}
		for _, other := range []string{o.output, o.jsonOutput} {
			if other != "" && sameAbsolutePath(other, o.trace) {
				return fmt.Errorf("trace must be a different path from output and json-output")
			}
		}
	}
	if o.debugBundle != "" || o.debugBundleLevel != debugbundle.LevelPublic {
		switch {
		case command != "diff":
			return fmt.Errorf("debug-bundle is supported only by diff")
		case o.debugBundle == "":
			return fmt.Errorf("debug-bundle-level requires --debug-bundle")
		case !slices.Contains(debugbundle.Levels, o.debugBundleLevel):
			return errors.New("debug-bundle-level must be " + orList(debugbundle.Levels))
		}
		for _, other := range []string{o.output, o.jsonOutput, o.trace} {
			if other != "" && (sameAbsolutePath(other, o.debugBundle) || sameAbsolutePath(other, debugNamesPath(o.debugBundle))) {
				return fmt.Errorf("debug-bundle and its names file must be different paths from output, json-output and trace")
			}
		}
	}
	if o.jsonOutput != "" {
		if command != "diff" || o.format == "json" {
			return fmt.Errorf("json-output requires diff with markdown, pr-markdown or terminal format")
		}
		if o.output != "" && sameAbsolutePath(o.output, o.jsonOutput) {
			return fmt.Errorf("output and json-output must be different paths")
		}
	}
	return nil
}

// rendersTargets reports whether the command renders base and head targets.
func (o *commandOptions) rendersTargets() bool {
	return o.command == "diff" || (o.command == "doctor" && o.renderCheck)
}
