package localhelm

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"diffaro/internal/determinism"
	"diffaro/internal/domain"
	"go.yaml.in/yaml/v3"
)

func trackingName(app domain.Application, namespace string) string {
	if app.ID.Namespace != namespace {
		return app.ID.Namespace + "_" + app.ID.Name
	}
	return app.ID.Name
}

// trackDocuments decodes raw, drops the hook documents helm template leaves
// out of template output when hooks is set (skipHookDocument) and adds Argo
// CD tracking (addTracking).
func trackDocuments(file, raw string, req Request, helm map[string]any, hooks bool) ([]string, error) {
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	out := []string{}
	for {
		obj, err := determinism.DecodeDocument(decoder)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, renderedDocumentFailure(file, "is not valid YAML")
		}
		if len(obj) == 0 {
			continue
		}
		if domain.String(obj, "kind") == "List" {
			return nil, skip("rendered Kubernetes Lists require list-aware tracking")
		}
		meta, _ := obj["metadata"].(map[string]any)
		if meta == nil {
			return nil, renderedDocumentFailure(file, "contains a document without metadata")
		}
		annotations, _ := meta["annotations"].(map[string]any)
		if hooks && skipHookDocument(annotations, helm["skipTests"] == true) {
			continue
		}
		addTracking(obj, meta, annotations, req)
		data, err := json.Marshal(obj)
		if err != nil {
			return nil, renderError()
		}
		out = append(out, string(data))
		if len(out) > 10000 {
			return nil, renderError()
		}
	}
	return out, nil
}

// skipHookDocument reports whether helm template leaves a template document
// out of its output. As in Helm's SortManifests, helm.sh/hook is a comma list
// of case-insensitive events and a document naming an unknown event is
// dropped; with --skip-tests a document naming test, or its Helm 2 alias
// test-success, is dropped too.
func skipHookDocument(annotations map[string]any, skipTests bool) bool {
	value, ok := annotations["helm.sh/hook"]
	if !ok {
		return false
	}
	events, _ := value.(string)
	test := false
	for event := range strings.SplitSeq(events, ",") {
		switch strings.ToLower(strings.TrimSpace(event)) {
		case "test", "test-success":
			test = true
		case "pre-install", "post-install", "pre-delete", "post-delete", "pre-upgrade", "post-upgrade", "pre-rollback", "post-rollback":
		default:
			return true
		}
	}
	return skipTests && test
}

// addTracking adds the configured Argo CD tracking annotation and label to a
// document other than a CustomResourceDefinition.
func addTracking(obj, meta, annotations map[string]any, req Request) {
	if domain.String(obj, "kind") == "CustomResourceDefinition" {
		return
	}
	if annotations == nil {
		annotations = map[string]any{}
	}
	name := trackingName(req.Application, req.Settings.ControllerNamespace)
	method := req.Settings.TrackingMethod
	if method == "annotation" || method == "annotation+label" {
		dest := req.Application.Destination()
		ns := domain.String(meta, "namespace")
		if ns == "" {
			ns = domain.String(dest, "namespace")
		}
		group, _, found := strings.Cut(domain.String(obj, "apiVersion"), "/")
		if !found {
			group = ""
		}
		annotations["argocd.argoproj.io/tracking-id"] = name + ":" + group + "/" + domain.String(obj, "kind") + ":" + ns + "/" + domain.String(meta, "name")
		if req.Settings.InstallationID != "" {
			annotations["argocd.argoproj.io/installation-id"] = req.Settings.InstallationID
		} else {
			delete(annotations, "argocd.argoproj.io/installation-id")
		}
		meta["annotations"] = annotations
	}
	if method == "label" || method == "annotation+label" {
		labels, _ := meta["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
		}
		if method == "annotation+label" && len(name) > 63 {
			name = strings.TrimRight(name[:63], "-_.")
		}
		labels[req.Settings.AppLabelKey] = name
		meta["labels"] = labels
	}
}

// renderedDocumentFailure names the rendered template file, never its content.
func renderedDocumentFailure(file, problem string) error {
	subject := "rendered output of " + file
	if file == "" {
		subject = "external Helm output"
	} else if !domain.SafeRelativePath(file) {
		return renderError()
	}
	return domain.FailReason(domain.CodeLocalRenderFailed, domain.ReasonRenderedOutputInvalid, "render", fmt.Sprintf("%s %s; content is withheld to protect values", subject, problem), "inspect that template and its Helm inputs for this Application and revision", nil)
}
