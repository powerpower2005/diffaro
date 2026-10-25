package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/reporter"
	"diffaro/internal/security"
)

// writeReport prints diagnostics, encodes the report in the requested format
// (plus JSON for --json-output), saves or prints it and returns the exit code.
func writeReport(report domain.Report, o *commandOptions, outputs []*reportOutput, redactor security.Redactor, stdout, stderr io.Writer) int {
	printReportDiagnostics(stderr, report, redactor, o.verbose)
	emission := reportEmission{format: o.format, json: o.jsonOutput != "", toStdout: o.output == "", quiet: o.quiet}
	if !emission.emit(report, outputs, redactor, stdout, stderr) {
		return domain.ExitFailed
	}
	if !o.quiet {
		printCompletion(stderr, report, redactor)
	}
	return report.ExitCode()
}

// reportEmission is how one report is delivered: the primary format with an
// optional banner before it, the --json-output JSON, and stdout when the
// primary report is not saved to a file.
type reportEmission struct {
	format, banner        string
	json, toStdout, quiet bool
	// stdoutFailure is printed when stdout cannot be written; empty prints
	// nothing.
	stdoutFailure string
	// withoutMarkers drops the PR Markdown integration marker (demo only).
	withoutMarkers bool
}

// emit redacts and encodes the report, saves the prepared outputs and prints
// the primary report to stdout. It returns false after a failure.
func (e reportEmission) emit(report domain.Report, outputs []*reportOutput, redactor security.Redactor, stdout, stderr io.Writer) bool {
	redacted, err := reporter.Redact(report, redactor)
	if err != nil {
		printEncodingFailure(stderr, err, redactor)
		return false
	}
	data, err := redacted.Encode(e.format)
	if err != nil {
		printEncodingFailure(stderr, err, redactor)
		return false
	}
	if e.withoutMarkers {
		data = reporter.WithoutIntegrationMarkers(data)
	}
	if e.banner != "" {
		data = append([]byte(e.banner), data...)
	}
	if e.json {
		jsonData, encodeErr := redacted.Encode("json")
		if encodeErr != nil {
			printEncodingFailure(stderr, encodeErr, redactor)
			return false
		}
		setOutputData(outputs, "json-output", jsonData)
	}
	setOutputData(outputs, "output", data)
	if !saveOutputs(outputs, stderr, redactor, e.quiet) {
		return false
	}
	if e.toStdout {
		if _, err = stdout.Write(data); err != nil {
			if e.stdoutFailure != "" {
				fmt.Fprintln(stderr, e.stdoutFailure)
			}
			return false
		}
	}
	return true
}

type reportOutput struct {
	option string
	path   string
	file   *os.File
	data   []byte
}

// setOutputData stages data for the prepared output of one option: "output",
// "json-output", "trace", "debug-bundle" or "debug-bundle names". An option
// without a prepared output is ignored.
func setOutputData(outputs []*reportOutput, option string, data []byte) {
	for _, output := range outputs {
		if output.option == option {
			output.data = data
		}
	}
}

// Prepare both destinations before contacting a provider. Existing reports are
// never truncated by the writeability probe or by staging a new report.
func prepareOutputs(primary, machine, trace string, bundles ...string) ([]*reportOutput, error) {
	outputs := []*reportOutput{}
	entries := []struct{ option, path string }{{"output", primary}, {"json-output", machine}, {"trace", trace}}
	for i, option := range []string{"debug-bundle", "debug-bundle names"} {
		if i < len(bundles) {
			entries = append(entries, struct{ option, path string }{option, bundles[i]})
		}
	}
	for _, entry := range entries {
		if entry.path == "" {
			continue
		}
		path, err := filepath.Abs(entry.path)
		if err != nil {
			return nil, fmt.Errorf("cannot prepare --%s: choose a valid file path", entry.option)
		}
		outputs = append(outputs, &reportOutput{option: entry.option, path: path})
	}
	for _, output := range outputs {
		if err := os.MkdirAll(filepath.Dir(output.path), 0700); err != nil {
			return nil, fmt.Errorf("cannot prepare --%s: choose a writable parent directory", output.option)
		}
	}
	type prepared struct {
		option string
		parent os.FileInfo
		name   string
		info   os.FileInfo
	}
	seen := []prepared{}
	for _, output := range outputs {
		parent, err := os.Stat(filepath.Dir(output.path))
		if err != nil {
			return nil, fmt.Errorf("cannot prepare --%s: cannot inspect the parent directory", output.option)
		}
		current := prepared{option: output.option, parent: parent, name: filepath.Base(output.path)}
		info, err := os.Lstat(output.path)
		if err == nil {
			current.info = info
		}
		for _, other := range seen {
			if (os.SameFile(other.parent, parent) && sameOutputPath(other.name, current.name)) || (other.info != nil && current.info != nil && os.SameFile(other.info, current.info)) {
				return nil, fmt.Errorf("%s and %s must be different files", other.option, output.option)
			}
		}
		seen = append(seen, current)
		if err == nil {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("cannot prepare --%s: destination must be a regular file, not a directory or symlink", output.option)
			}
			probe, openErr := os.OpenFile(output.path, os.O_WRONLY, 0600)
			if openErr != nil {
				return nil, fmt.Errorf("cannot prepare --%s: existing report is not writable", output.option)
			}
			if probe.Close() != nil {
				return nil, fmt.Errorf("cannot prepare --%s: cannot close the destination probe", output.option)
			}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("cannot prepare --%s: cannot inspect the destination", output.option)
		}
	}
	for _, output := range outputs {
		file, err := os.CreateTemp(filepath.Dir(output.path), ".diffaro-report-*.tmp")
		if err != nil {
			cleanupOutputs(outputs)
			return nil, fmt.Errorf("cannot prepare --%s: parent directory is not writable", output.option)
		}
		output.file = file
	}
	return outputs, nil
}

func cleanupOutputs(outputs []*reportOutput) {
	for _, output := range outputs {
		if output.file != nil {
			output.file.Close()
			os.Remove(output.file.Name())
		}
	}
}

func saveOutputs(outputs []*reportOutput, diagnostic io.Writer, redactor security.Redactor, quiet bool) bool {
	for _, output := range outputs {
		_, writeErr := output.file.Write(output.data)
		closeErr := output.file.Close()
		if writeErr != nil || closeErr != nil {
			fmt.Fprintf(diagnostic, "cannot write --%s; previous reports were preserved; check free space and directory permissions\n", output.option)
			return false
		}
	}
	for i, output := range outputs {
		if err := os.Rename(output.file.Name(), output.path); err != nil {
			fmt.Fprintf(diagnostic, "cannot save --%s; check whether the destination changed or is locked\n", output.option)
			for _, saved := range outputs[:i] {
				fmt.Fprintf(diagnostic, "Already saved --%s: %q\n", saved.option, redactor.Text(saved.path))
			}
			return false
		}
		if !quiet {
			fmt.Fprintf(diagnostic, "Saved --%s: %q\n", output.option, redactor.Text(output.path))
		}
	}
	return true
}

// sameAbsolutePath reports whether two output paths name the same file after
// making them absolute; a path that cannot be made absolute counts as equal.
func sameAbsolutePath(a, b string) bool {
	left, leftErr := filepath.Abs(a)
	right, rightErr := filepath.Abs(b)
	return leftErr != nil || rightErr != nil || sameOutputPath(left, right)
}

func sameOutputPath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
