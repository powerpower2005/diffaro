package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"

	"diffaro/internal/demo"
	"diffaro/internal/domain"
	"diffaro/internal/security"
)

// demoFormats are the report formats demo accepts, in display order.
var demoFormats = []string{"terminal", "markdown", "pr-markdown", "json"}

func runDemo(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("demo", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	scenario := f.String("scenario", "new-app", "embedded fixture: "+orList(demo.Scenarios))
	format := f.String("format", "terminal", "report format: "+orList(demoFormats))
	output := f.String("output", "", "save the primary demo report")
	jsonOutput := f.String("json-output", "", "save full JSON from the same demo comparison")
	if proceed, code := parseOptions(f, args, stdout, stderr); !proceed {
		return code
	}
	if err := demo.CheckScenario(*scenario); err != nil {
		fmt.Fprintln(stderr, err)
		return domain.ExitInvalidConfiguration
	}
	if !slices.Contains(demoFormats, *format) {
		fmt.Fprintln(stderr, "demo format must be "+orList(demoFormats))
		return domain.ExitInvalidConfiguration
	}
	if *jsonOutput != "" && *format == "json" {
		fmt.Fprintln(stderr, "json-output requires terminal, markdown or pr-markdown format")
		return domain.ExitInvalidConfiguration
	}
	outputs, err := prepareOutputs(*output, *jsonOutput, "")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return domain.ExitInvalidConfiguration
	}
	defer cleanupOutputs(outputs)
	report, err := demo.Run(ctx, *scenario)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return domain.ExitFailed
	}
	banner := ""
	switch *format {
	case "terminal":
		banner = "DEMO fixture (offline): " + *scenario + ". Synthetic Git/Argo context; actual bundled Helm. No credentials or network.\n\n"
	case "markdown", "pr-markdown":
		banner = "> **Offline demo fixture:** " + *scenario + ". Synthetic Git/Argo context; actual bundled Helm. No credentials or network.\n\n"
	}
	redactor := security.Redactor{}
	// The demo's PR Markdown carries no integration marker or sample SHAs in
	// comments, so it can never be taken for a PR result.
	emission := reportEmission{format: *format, banner: banner, json: *jsonOutput != "", toStdout: *output == "", stdoutFailure: "cannot write the demo report to stdout", withoutMarkers: *format == "pr-markdown"}
	if !emission.emit(report, outputs, redactor, stdout, stderr) {
		return domain.ExitFailed
	}
	fmt.Fprintln(stderr, "Offline demo: embedded fixtures and synthetic context only. For your own PR, see README.md#quick-start in the Diffaro repository.")
	printReportDiagnostics(stderr, report, redactor, false)
	printCompletion(stderr, report, redactor)
	return report.ExitCode()
}
