// Package cli implements the diffaro commands (demo, init, validate, doctor,
// diff) and is the composition root: it parses flags, loads the profile,
// resolves credentials, constructs adapters per Argo CD instance, runs
// preview, encodes outputs and returns the documented exit code.
//
// validate, doctor and diff share runProfileCommand: loadProfile reads the
// offline inputs, loadRunInputs resolves credentials and prepares output
// files, and runAndReport builds backends, runs the preview and writes the
// report. preview.Runner.Compare and preview.Finalize own every
// report and status decision after the run; init --endpoint runs one
// endpointInit method per mode.
//
// Stderr diagnostics are written by reporter.WriteDiagnostics and
// reporter.WriteIssue; this package bounds their bytes (diagnosticOutput)
// and chooses the verbosity.
//
// Domain decisions belong in preview, profile and the engines; this package
// should only wire them together and present results.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"diffaro/internal/debugbundle"
	"diffaro/internal/demo"
	"diffaro/internal/domain"
	"diffaro/internal/preview"
	"diffaro/internal/profile"
	"diffaro/internal/revision"
	"diffaro/internal/security"
)

// Run executes one diffaro command and returns its documented exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) (code int) {
	defer func() {
		// Last boundary: a panic outside the comparison, such as while
		// encoding, still ends with a bug notice and a failure exit code.
		if value := recover(); value != nil {
			printPanic(stderr, domain.PanicFailure(value, "cli"))
			code = domain.ExitFailed
		}
	}()
	if len(args) == 1 && args[0] == "--version" {
		printVersion(stdout)
		return domain.ExitSuccess
	}
	if len(args) == 2 && args[0] == "help" {
		return Run(ctx, []string{args[1], "--help"}, stdout, stderr)
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printUsage(stdout)
		return domain.ExitSuccess
	}
	switch args[0] {
	case "demo":
		return runDemo(ctx, args[1:], stdout, stderr)
	case "init":
		return initialize(ctx, args[1:], stdout, stderr)
	case "validate", "doctor", "diff":
		return runProfileCommand(ctx, args[0], args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, "unknown command; use diffaro help")
	return domain.ExitInvalidConfiguration
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, "Diffaro: read-only target manifest preview\nUsage: diffaro validate|doctor|diff [--config profile.yaml] (default: "+defaultConfig+", else "+initConfig+")\nInit: diffaro init "+initEndpointArgs+" [--single-instance] [--config NEW_FILE]\nDoctor: [--render [--pr NUMBER_OR_URL | --base REF --head REF] [--recursive]] [--mode NAME] [--verbose]\nDiff: --pr NUMBER_OR_URL OR --base REF --head REF [--mode NAME] [--recursive] [--format markdown|pr-markdown|json|terminal] [--output PATH] [--json-output PATH] [--trace PATH] [--debug-bundle PATH] [--quiet] [--verbose]\nHelp: diffaro COMMAND --help; Build: diffaro --version\nRemote Git references only. PR compares merge-base to head; no merge-result prediction.")
	fmt.Fprintln(out, "Multiple servers: diffaro init "+initConnectionsArgs)
	fmt.Fprintln(out, "New/reconfigured Git Helm children: init --local-helm or --helm-binary PATH; reuse the saved profile in diff/doctor and CI.")
	fmt.Fprintln(out, "Try without credentials: diffaro demo [--scenario "+strings.Join(demo.Scenarios, "|")+"]. First-use guide: README.md#quick-start in the Diffaro repository")
}

func runProfileCommand(ctx context.Context, command string, args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	o := bindCommandFlags(f, command)
	if proceed, code := parseOptions(f, args, stdout, stderr); !proceed {
		return code
	}
	f.Visit(func(v *flag.Flag) {
		o.formatSet = o.formatSet || v.Name == "format"
		o.flags = append(o.flags, v.Name)
	})
	if err := o.validate(); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return domain.ExitInvalidConfiguration
	}
	resolveConfigPath(o)
	c, pr, err := loadProfile(o)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return domain.ExitInvalidConfiguration
	}
	if command == "validate" {
		if err := checkRunProfile(c.Run); err != nil {
			fmt.Fprintln(stderr, err.Error())
			return domain.ExitInvalidConfiguration
		}
	} else if err := applyRunOptions(o, c.Run); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return domain.ExitInvalidConfiguration
	} else if err := o.validate(); err != nil {
		fmt.Fprintln(stderr, "profile run settings: "+err.Error())
		return domain.ExitInvalidConfiguration
	}
	if command == "validate" {
		fmt.Fprintln(stdout, "Configuration valid (offline; connectivity not checked).")
		for _, line := range describeRun(c.Run) {
			fmt.Fprintln(stdout, line)
		}
		return domain.ExitSuccess
	}
	in, err := loadRunInputs(f, o, c, pr)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return domain.ExitInvalidConfiguration
	}
	defer cleanupOutputs(in.outputs)
	return runAndReport(ctx, o, in, stdout, stderr)
}

// loadProfile reads the offline inputs: the profile with the Helm flag
// overrides applied, which validate checks, and the --pr number, which
// validate rejects.
func loadProfile(o *commandOptions) (profile.Config, int, error) {
	c, err := profile.Load(o.configPath)
	if errors.Is(err, fs.ErrNotExist) && !slices.Contains(o.flags, "config") {
		return c, 0, fmt.Errorf("no profile found: neither %s nor %s exists here; run diffaro init to create one, or pass --config FILE", defaultConfig, initConfig)
	}
	if err != nil {
		return c, 0, err
	}
	if err := applyHelmOverrides(&c, o); err != nil {
		return c, 0, err
	}
	pr, err := revision.PullRequestNumber(o.prInput, c.Git.RepositoryURL)
	return c, pr, err
}

// runInputs is everything doctor and diff resolve before contacting a
// provider.
type runInputs struct {
	config profile.Config // credentials resolved in memory
	pr     int
	// helmEnvironment holds each instance's resolved helmEnvironment.
	helmEnvironment map[string][]string
	// redactor masks every candidate credential and environment value.
	redactor security.Redactor
	// outputs are the prepared --output, --json-output and --trace files;
	// the caller must clean them up.
	outputs []*reportOutput
}

// loadRunInputs resolves credentials and helmEnvironment, rejects discovery
// without rendering and prepares the output files.
func loadRunInputs(f *flag.FlagSet, o *commandOptions, c profile.Config, pr int) (runInputs, error) {
	in := runInputs{pr: pr, helmEnvironment: map[string][]string{}}
	secrets, err := o.credentials.resolve(f, &c)
	if err != nil {
		return in, err
	}
	for _, instance := range c.Instances {
		if len(instance.HelmEnvironment) == 0 {
			continue
		}
		env, envSecrets, err := instance.ResolveHelmEnvironment(os.Getenv)
		secrets = append(secrets, envSecrets...)
		if err != nil {
			return in, err
		}
		in.helmEnvironment[instance.ID] = env
	}
	if c.Discovery != nil && !o.rendersTargets() {
		return in, errors.New("repository discovery requires doctor --render to verify candidate graphs; run diffaro doctor --render --pr NUMBER to check a PR")
	}
	names := ""
	if o.debugBundle != "" && o.debugBundleLevel == debugbundle.LevelPublic {
		names = debugNamesPath(o.debugBundle)
	}
	in.outputs, err = prepareOutputs(o.output, o.jsonOutput, o.trace, o.debugBundle, names)
	if err != nil {
		return in, err
	}
	in.config = c
	in.redactor = security.Redactor{Paths: c.SensitivePaths, Secrets: secrets}
	return in, nil
}

// runAndReport builds the backends, runs doctor or the comparison, writes
// the optional trace and the report, and returns the exit code.
func runAndReport(ctx context.Context, o *commandOptions, in runInputs, stdout, stderr io.Writer) int {
	var trace *domain.Trace
	if o.trace != "" || o.debugBundle != "" {
		trace = domain.NewTrace()
		ctx = domain.WithTrace(ctx, trace)
	}
	b := buildBackends(ctx, in.config, in.helmEnvironment)
	defer b.close()

	progress := startProgress(stderr, o.quiet, 5*time.Second)
	defer progress.close()
	report := runPreview(ctx, o, in, b, progress)
	report.Generator = newGenerator(o.command, o.mode, in.config)
	progress.close()
	if trace != nil {
		data, err := encodeTrace(report, trace, in.redactor)
		if err != nil {
			printEncodingFailure(stderr, err, in.redactor)
			return domain.ExitFailed
		}
		setOutputData(in.outputs, "trace", data)
		if o.debugBundle != "" && !stageDebugBundle(report, trace, data, o, in, stderr) {
			return domain.ExitFailed
		}
	}
	return writeReport(report, o, in.outputs, in.redactor, stdout, stderr)
}

// runPreview selects generated Applications, runs doctor or compare and
// finalizes the report. A panic outside the Application boundaries becomes a
// failed report, so the report and trace are still written.
func runPreview(ctx context.Context, o *commandOptions, in runInputs, b *backends, progress *progress) (report domain.Report) {
	defer func() {
		if value := recover(); value != nil {
			report = preview.RevisionFailureReport(ctx, in.config, o.recursive, domain.PanicFailure(value, "preview"))
		}
	}()
	progress.stage("selecting Applications")
	c, selections, snapshots, selectionIssues := preview.SelectGenerated(ctx, in.config, b.inventories)
	if !o.rendersTargets() {
		progress.stage("checking read-only access")
		report = preview.Doctor(ctx, c, b.git, b.renderers)
	} else {
		report = compare(ctx, c, b, o, in.pr, in.redactor, progress)
	}
	progress.stage("verifying selection and preparing the report")
	selectionIssues = append(selectionIssues, preview.VerifySelections(ctx, c, b.inventories, snapshots)...)
	preview.Finalize(ctx, &report, c, preview.FinalizeOptions{
		Selections: selections, SelectionIssues: selectionIssues, SetupIssues: b.setupIssues,
		Diff:         o.command == "diff",
		DoctorRender: o.command == "doctor" && o.renderCheck, VersionReaders: b.versionReaders,
	})
	return report
}
