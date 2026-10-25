package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/profile"
)

// init argument summaries shared by the usage, init --help and init errors.
const (
	initEndpointArgs    = "--endpoint URL [--pr URL | --application namespace/name | --application-set namespace/name]"
	initConnectionsArgs = "--connections CATALOG --pr URL [--local-children] --config NEW_FILE"
)

// orList joins two or more values as "a, b or c" for help and error text.
func orList(values []string) string {
	last := len(values) - 1
	return strings.Join(values[:last], ", ") + " or " + values[last]
}

func printHelp(f *flag.FlagSet, out io.Writer) {
	f.SetOutput(out)
	fmt.Fprintf(out, "Usage: diffaro %s [options]\n", f.Name())
	switch f.Name() {
	case "demo":
		fmt.Fprintln(out, "Offline: trusted embedded Helm fixtures; synthetic Git/Argo context. No config, token or network required.")
		fmt.Fprintln(out, "Exit codes match diff: 1 means observed changes; 3 means the deliberate partial-failure scenario retained useful results.")
	case "init":
		fmt.Fprintln(out, "Connect: "+initEndpointArgs+" --config NEW_FILE")
		fmt.Fprintln(out, "Multiple servers: "+initConnectionsArgs+" (offline setup)")
	case "diff":
		fmt.Fprintln(out, "Compare: [--config PROFILE] (--pr NUMBER_OR_URL | --base REF --head REF)")
		fmt.Fprintln(out, "PR compares merge-base to head target manifests. Exit 1 means changes found.")
	case "doctor":
		fmt.Fprintln(out, "Check: [--config PROFILE] [--render [--pr NUMBER_OR_URL | --base REF --head REF] [--recursive]]")
	case "validate":
		fmt.Fprintln(out, "Offline configuration check: [--config PROFILE]")
	}
	fmt.Fprintln(out, "Options:")
	supportedFlags(f).PrintDefaults()
}

// rejectedFlags are the shared profile command options each command rejects
// (commandOptions.validate). Help leaves them out, so it lists only options
// the command accepts.
var rejectedFlags = map[string][]string{
	"validate": {"base", "head", "pr", "format", "output", "json-output", "trace", "debug-bundle", "debug-bundle-level", "mode", "render", "recursive", "verbose"},
	"doctor":   {"json-output", "trace", "debug-bundle", "debug-bundle-level"},
	"diff":     {"render"},
}

// supportedFlags returns the options of f that its command accepts, with
// the report formats it accepts.
func supportedFlags(f *flag.FlagSet) *flag.FlagSet {
	shown := flag.NewFlagSet(f.Name(), flag.ContinueOnError)
	shown.SetOutput(f.Output())
	f.VisitAll(func(option *flag.Flag) {
		if slices.Contains(rejectedFlags[f.Name()], option.Name) {
			return
		}
		usage := option.Usage
		if option.Name == "format" && f.Name() == "doctor" {
			usage = "report format: markdown, json or terminal"
		}
		shown.Var(option.Value, option.Name, usage)
		// Var reads the default from the value, which holds whatever was
		// parsed before --help; help shows the declared default.
		shown.Lookup(option.Name).DefValue = option.DefValue
	})
	return shown
}

var safeFlagName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// Release packaging sets these with Go linker flags. Ordinary builds continue
// to use Go's own build metadata.
var releaseVersion string
var releaseRevision string

// Never echo supplied values or the flag package's raw parse diagnostics.
func parseOptions(f *flag.FlagSet, args []string, out, diagnostic io.Writer) (bool, int) {
	err := f.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		printHelp(f, out)
		return false, domain.ExitSuccess
	}
	if err != nil {
		message := "invalid arguments"
		for _, arg := range args {
			if !strings.HasPrefix(arg, "-") {
				continue
			}
			name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
			if !safeFlagName.MatchString(name) {
				continue
			}
			if f.Lookup(name) == nil {
				message = "invalid arguments: unknown option --" + name
				break
			}
		}
		fmt.Fprintf(diagnostic, "%s; use diffaro %s --help\n", message, f.Name())
		return false, domain.ExitInvalidConfiguration
	}
	if f.NArg() != 0 {
		fmt.Fprintf(diagnostic, "invalid arguments: unexpected positional arguments; use diffaro %s --help\n", f.Name())
		return false, domain.ExitInvalidConfiguration
	}
	return true, domain.ExitSuccess
}

func printVersion(out io.Writer) {
	build, _ := debug.ReadBuildInfo()
	version, revision := buildIdentity(releaseVersion, releaseRevision, build)
	fmt.Fprintf(out, "Diffaro %s; revision %s; %s\n", version, revision, runtime.Version())
}

// newGenerator identifies this build, the command, the run mode and the
// shape of c.
func newGenerator(command, mode string, c profile.Config) *domain.Generator {
	build, _ := debug.ReadBuildInfo()
	version, revision := buildIdentity(releaseVersion, releaseRevision, build)
	return &domain.Generator{Version: version, Revision: revision, GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH, Command: command, Mode: mode, Profile: c.Shape()}
}

func buildIdentity(tag, commit string, build *debug.BuildInfo) (string, string) {
	version, revision := "dev", "unknown"
	modified := false
	if build != nil {
		if build.Main.Version != "" && build.Main.Version != "(devel)" {
			version = build.Main.Version
		}
		for _, setting := range build.Settings {
			if setting.Key == "vcs.revision" {
				revision = setting.Value[:min(12, len(setting.Value))]
			}
			if setting.Key == "vcs.modified" && setting.Value == "true" {
				modified = true
			}
		}
	}
	if tag != "" {
		version = tag
	}
	if commit != "" {
		revision = commit[:min(12, len(commit))]
		modified = strings.HasSuffix(commit, "-dirty")
	}
	if modified {
		version += " (modified)"
	}
	return version, revision
}
