package cli

import (
	"io"

	"diffaro/internal/debugbundle"
	"diffaro/internal/domain"
	"diffaro/internal/reporter"
)

// debugNamesPath is where a public debug bundle's names file is written:
// next to the bundle, never inside it.
func debugNamesPath(bundle string) string { return bundle + ".names.json" }

// stageDebugBundle builds the --debug-bundle archive from the
// finished report, the trace steps redacted like the trace file and, for a
// private bundle, the redacted report and trace, and stages it with the
// other outputs. It returns false after printing a failure.
func stageDebugBundle(report domain.Report, trace *domain.Trace, traceJSON []byte, o *commandOptions, in runInputs, stderr io.Writer) bool {
	steps, dropped := trace.Steps()
	for i := range steps {
		steps[i].Subject, steps[i].Reason = in.redactor.Text(steps[i].Subject), in.redactor.Text(steps[i].Reason)
	}
	bundle := debugbundle.Input{Level: o.debugBundleLevel, Report: report, ExitCode: report.ExitCode(), Flags: o.flags, Steps: steps, Dropped: dropped}
	if o.debugBundleLevel == debugbundle.LevelPrivate {
		redacted, err := reporter.Redact(report, in.redactor)
		if err != nil {
			printEncodingFailure(stderr, err, in.redactor)
			return false
		}
		if bundle.ReportJSON, err = redacted.Encode("json"); err != nil {
			printEncodingFailure(stderr, err, in.redactor)
			return false
		}
		bundle.TraceJSON = traceJSON
	}
	out, err := debugbundle.Build(bundle)
	if err != nil {
		printEncodingFailure(stderr, err, in.redactor)
		return false
	}
	setOutputData(in.outputs, "debug-bundle", out.Bundle)
	setOutputData(in.outputs, "debug-bundle names", out.Names)
	return true
}
