package localhelm

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"diffaro/internal/domain"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/strvals"
)

// collectValues merges a source's Helm values like Argo CD: value files,
// valuesObject or inline values, parameters (forced strings last) and file
// parameters. resolved holds the source's input files (resolveInputs); each is
// read from files, the source's own repository, or from the ref repository it
// names. inputs, when set, receives the same inputs for a Helm CLI.
func collectValues(files map[string][]byte, resolved []inputFile, helm map[string]any, req Request, source domain.SourceRevision, inputs *helmCLIInputs) (map[string]any, error) {
	values, err := valueFileValues(files, resolved, helm, req, source, inputs)
	if err != nil {
		return nil, err
	}
	if values, err = mergeInlineValues(values, helm, inputs); err != nil {
		return nil, err
	}
	if err = setParameters(values, helm, req, source, inputs); err != nil {
		return nil, err
	}
	if err = setFileParameters(values, files, resolved, helm, req, inputs); err != nil {
		return nil, err
	}
	return values, nil
}

// readInput returns the bytes of a resolved input file and whether it exists.
func readInput(files map[string][]byte, req Request, in inputFile) ([]byte, bool, error) {
	if in.Malformed {
		return nil, false, renderError()
	}
	if in.Err != nil {
		return nil, false, in.Err
	}
	selected := files
	if in.Ref != "" {
		selected = req.Repositories[in.RepoKey]
	}
	data, ok := selected[in.Path]
	return data, ok, nil
}

// valueFileValues merges the value files in order. A missing file fails
// unless ignoreMissingValueFiles is set, and only the first occurrence of
// each resolved file is kept, like Argo CD.
func valueFileValues(files map[string][]byte, resolved []inputFile, helm map[string]any, req Request, source domain.SourceRevision, inputs *helmCLIInputs) (map[string]any, error) {
	// identity names the repository revision of a value file for
	// de-duplication: the first plan source declaring its ref, which differs
	// from RepoKey only for a ref named again after a source without files.
	identity := func(in inputFile) string {
		if in.Ref != "" {
			for _, candidate := range req.Plan {
				if "$"+candidate.Ref == in.Ref {
					return candidate.Repository + "@" + candidate.Revision
				}
			}
		}
		return source.Repository + "@" + source.Revision
	}
	values := map[string]any{}
	seen := map[string]bool{}
	for _, in := range resolved {
		if in.Kind != valueFileInput {
			continue
		}
		data, exists, err := readInput(files, req, in)
		if err != nil {
			return nil, err
		}
		if !exists {
			if helm["ignoreMissingValueFiles"] == true {
				continue
			}
			// Name the file like a missing chart path; the path is
			// repository-relative and comes from the Application spec.
			return nil, domain.FailReason(domain.CodeLocalRenderFailed, domain.ReasonValueFileMissing, "render", "value file "+strconv.Quote(in.Path)+" does not exist at this revision", "check the Application valueFiles and whether the file exists at this revision; Argo CD fails a missing value file unless ignoreMissingValueFiles is set", nil)
		}
		key := identity(in) + ":" + in.Path
		if seen[key] {
			continue
		}
		seen[key] = true
		if inputs != nil {
			inputs.valueFiles = append(inputs.valueFiles, data)
		}
		parsed, err := loader.LoadValues(bytes.NewReader(data))
		if err != nil {
			return nil, renderError()
		}
		values = loader.MergeMaps(values, parsed)
	}
	return values, nil
}

// mergeInlineValues merges valuesObject, or else the inline values string.
func mergeInlineValues(values map[string]any, helm map[string]any, inputs *helmCLIInputs) (map[string]any, error) {
	var data []byte
	if object, ok := helm["valuesObject"].(map[string]any); ok {
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, renderError()
		}
		data = encoded
	} else if inline := domain.String(helm, "values"); inline != "" {
		data = []byte(inline)
	} else {
		return values, nil
	}
	if inputs != nil {
		inputs.valueFiles = append(inputs.valueFiles, data)
	}
	parsed, err := loader.LoadValues(bytes.NewReader(data))
	if err != nil {
		return nil, renderError()
	}
	return loader.MergeMaps(values, parsed), nil
}

// setParameters applies helm.parameters, forced strings after the others,
// with build environment variables expanded.
func setParameters(values map[string]any, helm map[string]any, req Request, source domain.SourceRevision, inputs *helmCLIInputs) error {
	params, _ := helm["parameters"].([]any)
	if err := validateParameterNames(params); err != nil {
		return err
	}
	for _, force := range []bool{false, true} {
		for _, raw := range params {
			p, ok := raw.(map[string]any)
			if !ok {
				return renderError()
			}
			if (p["forceString"] == true) != force {
				continue
			}
			name, value := domain.String(p, "name"), domain.String(p, "value")
			value, err := buildEnv(value, req, source)
			if err != nil {
				return err
			}
			value = cleanParameter(value)
			if inputs != nil {
				if force {
					inputs.stringParameters = append(inputs.stringParameters, name+"="+value)
				} else {
					inputs.parameters = append(inputs.parameters, name+"="+value)
				}
			}
			if force {
				err = strvals.ParseIntoString(name+"="+value, values)
			} else {
				err = strvals.ParseInto(name+"="+value, values)
			}
			if err != nil {
				return renderError()
			}
		}
	}
	return nil
}

// setFileParameters sets each file parameter to the contents of its file,
// which must exist.
func setFileParameters(values map[string]any, files map[string][]byte, resolved []inputFile, helm map[string]any, req Request, inputs *helmCLIInputs) error {
	fileParams, _ := helm["fileParameters"].([]any)
	if err := validateParameterNames(fileParams); err != nil {
		return err
	}
	for _, in := range resolved {
		if in.Kind != fileParameterInput {
			continue
		}
		data, exists, err := readInput(files, req, in)
		if err != nil {
			return err
		}
		if !exists {
			return renderError()
		}
		if inputs != nil {
			inputs.fileParameters = append(inputs.fileParameters, helmCLIFileParameter{name: in.Name, data: data})
		}
		if err = strvals.ParseIntoFile(in.Name+"=file", values, func([]rune) (any, error) { return string(data), nil }); err != nil {
			return renderError()
		}
	}
	return nil
}

func buildEnv(raw string, req Request, source domain.SourceRevision) (string, error) {
	dest := req.Application.Destination()
	short := func(n int) string {
		if len(source.Revision) < n {
			return source.Revision
		}
		return source.Revision[:n]
	}
	values := map[string]string{
		"ARGOCD_APP_NAME":           trackingName(req.Application, req.Settings.ControllerNamespace),
		"ARGOCD_APP_NAMESPACE":      domain.String(dest, "namespace"),
		"ARGOCD_APP_PROJECT_NAME":   req.Application.Project(),
		"ARGOCD_APP_REVISION":       source.Revision,
		"ARGOCD_APP_REVISION_SHORT": short(7), "ARGOCD_APP_REVISION_SHORT_8": short(8),
		"ARGOCD_APP_SOURCE_REPO_URL":        source.Repository,
		"ARGOCD_APP_SOURCE_PATH":            domain.String(source.Source, "path"),
		"ARGOCD_APP_SOURCE_TARGET_REVISION": source.Revision,
	}
	unknown := false
	result := os.Expand(raw, func(name string) string {
		if name == "$" {
			return "$"
		}
		value, ok := values[name]
		if !ok {
			unknown = true
		}
		return value
	})
	if unknown {
		return "", skip("input uses unavailable build environment variables")
	}
	return result, nil
}

func cleanParameter(value string) string {
	if strings.HasPrefix(value, "{") && strings.HasSuffix(value, "}") {
		return value
	}
	var out strings.Builder
	var previous rune
	for _, r := range value {
		if r == ',' && previous != '\\' {
			out.WriteRune('\\')
		}
		out.WriteRune(r)
		previous = r
	}
	return out.String()
}

func validateParameterNames(items []any) error {
	if len(items) > 1000 {
		return renderError()
	}
	names := []string{}
	seen := map[string]bool{}
	for _, item := range items {
		p, ok := item.(map[string]any)
		if !ok {
			return renderError()
		}
		name := domain.String(p, "name")
		if name == "" {
			return renderError()
		}
		if !singleAssignmentKey(name) {
			return domain.FailReason(domain.CodeLocalRenderFailed, domain.ReasonParameterInvalid, "render", "Helm parameter name contains an unescaped '=' or ',' that Helm would parse as another assignment", "escape the character with a backslash or rename the parameter; Diffaro refuses names that change Helm --set/--set-file parsing", nil)
		}
		if seen[name] {
			continue
		}
		for _, existing := range names {
			for _, pair := range [][2]string{{name, existing}, {existing, name}} {
				if strings.HasPrefix(pair[1], pair[0]+".") || strings.HasPrefix(pair[1], pair[0]+"[") {
					return skip("overlapping parameter paths have backend-dependent application order")
				}
			}
		}
		seen[name] = true
		names = append(names, name)
	}
	return nil
}

// singleAssignmentKey reports whether Helm's strvals parser reads name as one
// key. Helm ends a key at an unescaped '=' and starts another assignment at an
// unescaped ','; with --set-file that second assignment reads any runner path.
// A backslash escapes the next rune, as in `nginx\.ingress`.
func singleAssignmentKey(name string) bool {
	escaped := false
	for _, r := range name {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '=' || r == ',':
			return false
		}
	}
	return !escaped
}
