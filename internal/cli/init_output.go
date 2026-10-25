package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"strings"

	"diffaro/internal/domain"
	"diffaro/internal/preview"
	"diffaro/internal/profile"
	"diffaro/internal/security"
)

func writeInitializedProfile(path string, data []byte, configuration profile.Config, generated, single bool, prURL string, stdout, stderr io.Writer) int {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		fmt.Fprintln(stderr, "cannot create configuration file")
		return domain.ExitFailed
	}
	_, err = file.Write(data)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		fmt.Fprintln(stderr, "cannot finish writing configuration file; inspect the partial file")
		return domain.ExitFailed
	}
	if runtime.GOOS == "windows" {
		fmt.Fprintln(stdout, "Profile created; available credentials were saved. Windows directory ACLs control access; keep this profile in a directory restricted to your account.")
	} else {
		fmt.Fprintln(stdout, "Profile created with private file permissions (0600); available credentials were saved.")
	}
	writeInitNextSteps(stdout, configuration, path, prURL)
	if generated {
		fmt.Fprintln(stdout, preview.GeneratedSelectionBoundary)
	}
	if single {
		fmt.Fprintln(stdout, "Recursive default routing explicitly selects this single instance; review before using multi-instance graphs.")
	}
	return domain.ExitSuccess
}

func writeInitNextSteps(out io.Writer, configuration profile.Config, path, prURL string) {
	redactor := security.Redactor{Secrets: []string{configuration.Git.Token, os.Getenv(configuration.Git.TokenEnv)}}
	for _, connection := range configuration.Instances {
		redactor.Secrets = append(redactor.Secrets, connection.Token, os.Getenv(connection.TokenEnv))
	}
	fmt.Fprintln(out, "Authentication: explicit token flags > saved config tokens > tokenEnv fallback. Saved credentials can be reused without init; runtime overrides do not update the file.")
	quotedPath := shellArgument(path, runtime.GOOS)
	doctorPR, diffPR := "", "PR_NUMBER_OR_URL"
	if prURL != "" {
		doctorPR = " --pr " + shellArgument(prURL, runtime.GOOS)
		diffPR = shellArgument(prURL, runtime.GOOS)
	}
	var message strings.Builder
	message.WriteString("Credential fallback environment variables: ")
	for _, connection := range configuration.Instances {
		fmt.Fprintf(&message, "%s (Argo CD instance %s), ", connection.TokenEnv, connection.ID)
	}
	fmt.Fprintf(&message, "%s (GitHub; a read-only token is recommended even for public repositories, because GitHub.com allows 60 unauthenticated API requests per hour).\n", configuration.Git.TokenEnv)
	if configuration.Git.APIURL != "https://api.github.com" {
		// Derived from the repository host unless --git-api named it.
		fmt.Fprintf(&message, "GitHub API: %s; rerun init with --git-api URL if your server uses another.\n", redactor.Text(configuration.Git.APIURL))
	}
	fmt.Fprintf(&message, "Render limits: %s per stage; %d concurrent renders; %d Applications; %d bytes per API/manifest input; %d documents/resources per target.\n", configuration.Render.Timeout, configuration.Render.MaxConcurrency, configuration.Render.MaxApplications, configuration.Render.MaxResponseBytes, configuration.Render.MaxDocuments)
	if shadowedByDefault(path) {
		fmt.Fprintf(&message, "Note: %s in this directory takes precedence over %s when --config is not given, so the commands below name the new profile.\n", defaultConfig, initConfig)
	}
	if starterRun(configuration.Run) {
		// The run section supplies the recursion, the format and the ci
		// outputs; the profile read without --config needs none.
		config := " --config " + quotedPath
		if path == defaultConfig || (path == initConfig && !shadowedByDefault(path)) {
			config = ""
		}
		fmt.Fprintf(&message, "Next:\n  diffaro doctor%s --render%s\n  diffaro diff%s --pr %s\n  diffaro diff%s --mode ci --pr %s\n", config, doctorPR, config, diffPR, config, diffPR)
		message.WriteString("The profile's run section makes diff a recursive terminal report, including App-of-Apps children; --mode ci saves preview.md (PR Markdown), preview.json, trace.json and a private debug bundle. Edit the section to change them; diffaro validate prints each mode's options, and explicit flags override them.\n")
	} else {
		recursive := " --recursive"
		fmt.Fprintf(&message, "Next:\n  diffaro doctor --config %s --render%s%s --format terminal\n  diffaro diff --config %s --pr %s%s --format terminal --json-output preview.json\n", quotedPath, doctorPR, recursive, quotedPath, diffPR, recursive)
	}
	if prURL == "" {
		message.WriteString("Replace PR_NUMBER_OR_URL with the PR to review.\n")
	}
	if configuration.Discovery != nil && configuration.Discovery.Selection != profile.SelectionChangedPaths {
		message.WriteString("Selection: every candidate Application is rendered on each run. In a large repository that takes long and can exhaust a shared API quota; discovery.selection: changedPaths, the init default, renders only the Applications the PR's files can affect.\n")
	}
	for _, connection := range configuration.Instances {
		fmt.Fprintf(&message, "Instance %s: ", connection.ID)
		if connection.LocalHelm {
			if connection.HelmBinary != "" {
				message.WriteString("Render setup: operator-selected external Helm; doctor --render checks the executable, version and target inputs. No repo-server connection or Kubernetes tools. See docs/configuration.md#local-helm in the Diffaro repository.\n")
				if len(connection.HelmEnvironment) == 0 {
					message.WriteString("  The executable receives no environment. If its template functions need one, such as a service endpoint, add it with --helm-env NAME=VALUE or --helm-env-from NAME=ENV; otherwise those renders fail as helm_execution_failed.\n")
				}
			} else {
				message.WriteString("Render setup: bundled local Helm with GitHub and Argo CD read tokens; no repo-server connection or Kubernetes tools. Review skipped context and run doctor --render. See docs/configuration.md#local-helm in the Diffaro repository.\n")
			}
		} else if connection.RepoServer == nil {
			message.WriteString("Render setup: Application API only (basic analysis). New or reconfigured child resource analysis is skipped with an explicit reason; optional detailed analysis: select --local-helm or --helm-binary PATH on diff/doctor to analyze supported Git Helm children, or use a trusted repo-server backend. See README.md#quick-start in the Diffaro repository.\n")
		} else {
			message.WriteString("Render setup: repo-server configured; doctor --render checks actual access and rendering.\n")
		}
	}
	message.WriteString("Exit 1 means changes found; exit 3 means partial failure. Inspect the report status before integration.\n")
	message.WriteString("Reuse this profile for another PR in the same repository; change --pr, without rerunning init.")
	if !starterRun(configuration.Run) {
		message.WriteString(" For a saved PR report, use --format pr-markdown --output preview.md --json-output preview.json.")
	}
	message.WriteString("\n")
	fmt.Fprint(out, redactor.Text(message.String()))
}

// Commands are display text, never executed. Single quoting prevents shell
// expansion when a user copies paths or URLs containing spaces or metacharacters.
func shellArgument(value, platform string) string {
	if platform == "windows" {
		return "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// starterRun reports whether run holds what the short next steps rely on:
// recursive terminal defaults and a ci mode.
// shadowedByDefault reports whether path is the init profile while a
// diffaro.yaml, which resolveConfigPath prefers, exists.
func shadowedByDefault(path string) bool {
	if path != initConfig {
		return false
	}
	_, err := os.Stat(defaultConfig)
	return !errors.Is(err, fs.ErrNotExist)
}

func starterRun(run profile.Run) bool {
	_, ci := run.Modes["ci"]
	return ci && run.Defaults.Format == "terminal" && run.Defaults.Recursive != nil && *run.Defaults.Recursive
}
