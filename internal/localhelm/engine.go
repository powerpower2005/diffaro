package localhelm

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"diffaro/internal/argocd"
	"diffaro/internal/domain"
	"diffaro/internal/helmerror"
	"helm.sh/helm/v4/pkg/chart/common"
	chartutil "helm.sh/helm/v4/pkg/chart/common/util"
	"helm.sh/helm/v4/pkg/chart/loader/archive"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	dependencyutil "helm.sh/helm/v4/pkg/chart/v2/util"
	"helm.sh/helm/v4/pkg/engine"
	"helm.sh/helm/v4/pkg/ignore"
)

// Request is one local render: the source plan with each source's spec, the
// repository files keyed by repository@revision and the Argo CD render
// context. The Worker receives it as a workerRequest.
type Request struct {
	Application  domain.Application
	Plan         []domain.SourceRevision
	Repositories map[string]map[string][]byte
	Settings     argocd.RenderSettings
	Capabilities argocd.ClusterCapabilities
	Limit        int64
}

var errNondeterministic = errors.New("non-deterministic or live-dependent template function")

// nondeterministicFunctions are the template functions whose result depends
// on randomness, time or live state. The bundled engine fails a render that
// calls one (nondeterministicFuncs); for an external executable they are
// screened by name (unsafeFunction). lookup is not one of them: like helm
// template and Argo CD's repo-server, it returns an empty result without a
// cluster.
var nondeterministicFunctions = []string{"now", "ago", "randAlphaNum", "randAlpha", "randAscii", "randNumeric", "randBytes", "randInt", "uuidv4", "shuffle", "genCA", "genCAWithKey", "genSelfSignedCert", "genSelfSignedCertWithKey", "genSignedCert", "genSignedCertWithKey", "genPrivateKey", "encryptAES", "bcrypt", "htpasswd", "getHostByName"}

// nondeterministicFuncs is shared read-only; the Helm engine copies it into
// its own function map for every render.
var nondeterministicFuncs = func() template.FuncMap {
	funcs := template.FuncMap{}
	for _, fn := range nondeterministicFunctions {
		funcs[fn] = func(...any) (any, error) { return nil, errNondeterministic }
	}
	return funcs
}()

// Render uses upstream Helm for parsing, precedence, dependency values and
// templating. It does not execute Helm plugins or connect to Kubernetes.
func Render(req Request) (manifests []string, err error) {
	return render(req, nil)
}

type helmCLIInputs struct {
	valueFiles                   [][]byte
	parameters, stringParameters []string
	fileParameters               []helmCLIFileParameter
}
type helmCLIFileParameter struct {
	name string
	data []byte
}

// chartRenderer renders one source with an external engine. probe renders a
// freshly loaded copy of the same chart and values with the bundled engine
// without recovering failed templates.
type chartRenderer func(ch *chart.Chart, values map[string]any, helm map[string]any, req Request, inputs helmCLIInputs, probe func() error) (string, error)

func render(req Request, external chartRenderer) (manifests []string, err error) {
	var activeSource *domain.SourceRevision
	defer func() {
		if err != nil && activeSource != nil && !partial(err) {
			err = domain.SourceFailure(err, *activeSource)
		}
	}()
	if req.Limit < domain.MinRenderResponseBytes || req.Limit > domain.MaxRenderResponseBytes {
		return nil, renderError()
	}
	// A "$name/" input prefix selects a ref source only when its repository
	// files are present; a second source with the same ref is invalid then.
	refs := map[string]int{}
	for i, s := range req.Plan {
		activeSource = &s
		if s.Ref != "" {
			if _, ok := refs["$"+s.Ref]; ok {
				return nil, renderError()
			}
			if req.Repositories[s.Repository+"@"+s.Revision] != nil {
				refs["$"+s.Ref] = i
			}
		}
	}
	resolved := resolveInputs(req, refs)
	targets := &targetSet{docs: []string{}, limit: req.Limit}
	var partialIssues []domain.Issue
	for i, s := range req.Plan {
		activeSource = &s
		if s.Ref != "" && domain.String(s.Source, "path") == "" {
			continue
		}
		partialErr, err := renderSource(req, s, sourceInputs(resolved, i), external, targets)
		if err != nil {
			return nil, err
		}
		for _, issue := range domain.RenderIssues(partialErr) {
			issue.SourcePosition = s.Position
			issue.Repository = s.Repository
			issue.Revision = s.Revision
			partialIssues = append(partialIssues, issue)
		}
	}
	return targets.docs, domain.PartialRender(partialIssues)
}

// targetSet collects the tracked target documents of every source within
// the render.maxResponseBytes bound.
type targetSet struct {
	docs         []string
	total, limit int64
}

func (t *targetSet) add(docs []string) error {
	for _, doc := range docs {
		t.total += int64(len(doc))
		if t.total > t.limit {
			return domain.Fail(domain.CodeLimitExceeded, "render", "local target manifests exceed render.maxResponseBytes", "reduce the target or adjust the configured limit", nil)
		}
		t.docs = append(t.docs, doc)
	}
	return nil
}

// renderSource loads one Git Helm source's chart and values and adds its
// targets with the bundled engine or, when set, the external one. It returns
// the template failures recovered on the way as a partial render error.
func renderSource(req Request, s domain.SourceRevision, inputs []inputFile, external chartRenderer, targets *targetSet) (partialErr, err error) {
	files := req.Repositories[s.Repository+"@"+s.Revision]
	if len(files) == 0 {
		return nil, renderError()
	}
	if hasSourceOverride(files, domain.String(s.Source, "path"), req.Application.ID) {
		return nil, skip("repository source overrides are not supported by the local backend")
	}
	helm, ok := s.Source["helm"].(map[string]any)
	if !ok {
		return nil, renderError()
	}
	if domain.UsesNonHelmSource(s.Source) {
		return nil, skip("only explicit Git Helm sources can render locally")
	}
	if fields := unsupportedHelmFields(helm); len(fields) > 0 {
		return nil, skip("Helm source fields not supported by the local backend: " + strings.Join(fields, ", "))
	}
	if v := domain.String(helm, "version"); v != "" && v != "v4" && v != "v3" {
		return nil, skip("requested Helm version is unsupported")
	}
	root := domain.String(s.Source, "path")
	if !domain.SafeRelativePath(root) {
		return nil, renderError()
	}
	files, err = expandVendoredArchives(files, root, req.Limit)
	if err != nil {
		return nil, err
	}
	ch, err := loadChart(files, root)
	if err != nil {
		return nil, err
	}
	if ch.Metadata.Type == "library" {
		return nil, renderError()
	}
	if err = loadLocalDependencies(ch, files, root, 0); err != nil {
		return nil, err
	}
	cliInputs := helmCLIInputs{}
	values, err := collectValues(files, inputs, helm, req, s, &cliInputs)
	if err != nil {
		return nil, err
	}
	if external == nil {
		return bundledTargets(ch, values, helm, req, targets)
	}
	probe := func() error {
		fresh, err := loadChart(files, root)
		if err != nil {
			return err
		}
		if err = loadLocalDependencies(fresh, files, root, 0); err != nil {
			return err
		}
		probeValues, err := collectValues(files, inputs, helm, req, s, &helmCLIInputs{})
		if err != nil {
			return err
		}
		_, _, err = bundledTemplates(fresh, probeValues, helm, req, false)
		return err
	}
	raw, partialErr := external(ch, values, helm, req, cliInputs, probe)
	if partialErr != nil && !partial(partialErr) {
		return nil, partialErr
	}
	// The executable's output passes the same hook filter, so tests are
	// dropped without depending on version-specific --skip-tests behavior.
	docs, err := trackDocuments("", raw, req, helm, true)
	if err != nil {
		return nil, err
	}
	return partialErr, targets.add(docs)
}

// bundledTargets renders ch with the bundled engine and adds its tracked
// documents, CRDs last unless skipped, in template file order.
func bundledTargets(ch *chart.Chart, values map[string]any, helm map[string]any, req Request, targets *targetSet) (partialErr, err error) {
	rendered, reasons, err := bundledTemplates(ch, values, helm, req, true)
	if err != nil {
		return nil, err
	}
	if len(reasons) > 0 {
		partialErr = partialFailure(reasons)
	}
	keys := slices.Sorted(maps.Keys(rendered))
	templates := len(keys)
	if helm["skipCrds"] != true {
		for _, crd := range ch.CRDObjects() {
			keys = append(keys, crd.Filename)
			rendered[crd.Filename] = string(crd.File.Data)
		}
	}
	for i, key := range keys {
		if strings.HasSuffix(key, "/NOTES.txt") {
			continue
		}
		// helm template sorts hooks out of template output only and prints
		// crds/ files as they are.
		docs, err := trackDocuments(key, rendered[key], req, helm, i < templates)
		if err != nil {
			return nil, err
		}
		if err = targets.add(docs); err != nil {
			return nil, err
		}
	}
	return partialErr, nil
}

// bundledTemplates renders ch with the bundled Helm engine. Randomness, time
// and live-dependent functions fail the render when a template invokes them.
// With recover, a failing template is removed and the rest render again,
// returning the failures as reasons; without it the first failure ends the
// render.
func bundledTemplates(ch *chart.Chart, values map[string]any, helm map[string]any, req Request, recover bool) (map[string]string, []string, error) {
	if err := dependencyutil.ProcessDependencies(ch, common.Values(values)); err != nil {
		return nil, nil, renderError()
	}
	release := releaseContext(helm, req)
	caps := common.DefaultCapabilities.Copy()
	kv, err := common.ParseKubeVersion(release.kubeVersion)
	if err != nil {
		return nil, nil, skip("Kubernetes version is not available for local rendering")
	}
	caps.KubeVersion = *kv
	caps.HelmVersion.Version = HelmVersion
	if release.invalidAPIs {
		return nil, nil, renderError()
	}
	// helm template appends the requested APIs to its compiled defaults.
	caps.APIVersions = append(caps.APIVersions, release.apis...)
	if dependencyutil.ValidateReleaseName(release.name) != nil {
		return nil, nil, renderError()
	}
	if hasRemoteSchema(ch) {
		return nil, nil, skip("remote JSON schema references require external access")
	}
	renderValues, err := chartutil.ToRenderValuesWithSchemaValidation(ch, values, common.ReleaseOptions{Name: release.name, Namespace: release.namespace, Revision: 1, IsInstall: true}, caps, helm["skipSchemaValidation"] == true)
	if err != nil {
		return nil, nil, renderError()
	}
	rendered, err := (engine.Engine{CustomTemplateFuncs: nondeterministicFuncs}).Render(ch, renderValues)
	reasons := []string{}
	var index templateIndex
	for err != nil {
		raw := err.Error()
		if strings.Contains(raw, errNondeterministic.Error()) {
			reason := "template invokes randomness, time or live-dependent functions"
			if file, line, ok := helmerror.Location(raw); ok {
				reason = "template " + file + ":" + line + " invokes randomness, time or live-dependent functions"
			}
			return nil, nil, nondeterministicSkip(reason)
		}
		diagnostic := templateFailure(ch, raw)
		if !recover || len(reasons) >= 16 {
			return nil, nil, diagnostic
		}
		if _, ok := recoverTemplate(ch, raw, &index); !ok {
			return nil, nil, diagnostic
		}
		reasons = append(reasons, diagnostic.Error())
		rendered, err = (engine.Engine{CustomTemplateFuncs: nondeterministicFuncs}).Render(ch, renderValues)
	}
	return rendered, reasons, nil
}

// release is the Helm release context of one source for both engines: the
// release name, namespace, Kubernetes version and API versions with the
// Application's helm overrides applied. Each engine validates it in its own
// order.
type release struct {
	name, namespace, kubeVersion string
	// apis are the destination's API versions, replaced by a non-empty
	// helm.apiVersions; invalidAPIs marks an entry there that is not a string.
	apis        []string
	invalidAPIs bool
}

func releaseContext(helm map[string]any, req Request) release {
	r := release{name: domain.String(helm, "releaseName"), namespace: domain.String(req.Application.Destination(), "namespace"), kubeVersion: req.Capabilities.Version}
	if r.name == "" {
		r.name = req.Application.ID.Name
	}
	if override := domain.String(helm, "namespace"); override != "" {
		r.namespace = override
	}
	if r.namespace == "" {
		r.namespace = "default"
	}
	if override := domain.String(helm, "kubeVersion"); override != "" {
		r.kubeVersion = override
	}
	r.apis = append([]string(nil), req.Capabilities.APIVersions...)
	if extra, ok := helm["apiVersions"].([]any); ok && len(extra) > 0 {
		r.apis = nil
		for _, v := range extra {
			s, ok := v.(string)
			if !ok {
				r.invalidAPIs = true
				break
			}
			r.apis = append(r.apis, s)
		}
	}
	return r
}

var helmFieldName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,63}$`)

// unsupportedHelmFields returns, sorted, the helm block keys that are not
// Argo CD Helm source fields, so an input local Helm would not apply is never
// dropped silently. A key that is not a plain field name is not quoted.
func unsupportedHelmFields(helm map[string]any) []string {
	var fields []string
	for key := range helm {
		if slices.Contains(domain.HelmSourceFields, key) {
			continue
		}
		if !helmFieldName.MatchString(key) {
			key = "(a field with an invalid name)"
		}
		fields = append(fields, key)
	}
	slices.Sort(fields)
	return slices.Compact(fields)
}

// hasSourceOverride follows Argo CD's lookup: only the source path's
// .argocd-source.yaml and this Application's .argocd-source-<name>.yaml apply.
// Override files elsewhere in the repository do not affect this source.
func hasSourceOverride(files map[string][]byte, root string, id domain.ApplicationID) bool {
	names := []string{".argocd-source.yaml", ".argocd-source-" + id.Name + ".yaml", ".argocd-source-" + id.Namespace + "_" + id.Name + ".yaml"}
	if id.Name == "" {
		// Without an identity, any app-specific override in this path may apply.
		for name := range files {
			if path.Dir(name) == path.Clean(root) && strings.HasPrefix(path.Base(name), ".argocd-source") {
				return true
			}
		}
		return false
	}
	for _, name := range names {
		if _, ok := files[path.Join(root, name)]; ok {
			return true
		}
	}
	return false
}

func loadChart(files map[string][]byte, root string) (*chart.Chart, error) {
	rules := ignore.Empty()
	if data, ok := files[path.Join(root, ".helmignore")]; ok {
		var err error
		rules, err = ignore.Parse(bytes.NewReader(data))
		if err != nil {
			return nil, renderError()
		}
	}
	rules.AddDefaults()
	buffered := []*archive.BufferedFile{}
	var keys []string
	for name := range files {
		if root == "." || strings.HasPrefix(name, root+"/") {
			keys = append(keys, name)
		}
	}
	slices.Sort(keys)
	for _, name := range keys {
		rel := strings.TrimPrefix(name, root+"/")
		if root == "." {
			rel = name
		}
		if !domain.SafeRelativePath(rel) {
			return nil, renderError()
		}
		excluded := false
		for p := path.Dir(rel); p != "."; p = path.Dir(p) {
			if rules.Ignore(p, memoryInfo{name: p, dir: true}) {
				excluded = true
				break
			}
		}
		if excluded || rules.Ignore(rel, memoryInfo{name: rel}) {
			continue
		}
		buffered = append(buffered, &archive.BufferedFile{Name: rel, Data: files[name]})
	}
	// Like Argo CD's "app path does not exist", name a missing chart instead
	// of reporting a generic invalid input. The path is repository-relative.
	if !slices.ContainsFunc(buffered, func(f *archive.BufferedFile) bool { return f.Name == "Chart.yaml" }) {
		reasonCode, reason := domain.ReasonChartMetadataMissing, "chart path "+strconv.Quote(root)+" has no Chart.yaml at this revision"
		if len(buffered) == 0 {
			reasonCode, reason = domain.ReasonChartPathMissing, "chart path "+strconv.Quote(root)+" does not exist at this revision"
		}
		return nil, domain.FailReason(domain.CodeLocalRenderFailed, reasonCode, "render", reason, "check the Application source path and targetRevision; Argo CD reports \"app path does not exist\" for a missing source path", nil)
	}
	ch, err := loader.LoadFiles(buffered)
	if err != nil {
		return nil, renderError()
	}
	if ch.Metadata.Type != "" && ch.Metadata.Type != "application" && ch.Metadata.Type != "library" {
		return nil, renderError()
	}
	return ch, nil
}

// Vendored and same-repository file dependencies remain fully offline.
func loadLocalDependencies(ch *chart.Chart, files map[string][]byte, root string, depth int) error {
	if depth > 16 {
		return renderError()
	}
	for _, dep := range ch.Metadata.Dependencies {
		if dependencyLoaded(ch, dep.Name) {
			continue
		}
		depRoot, local := fileDependencyDir(root, dep.Repository)
		if !local {
			return skip("an unvendored Helm dependency requires a supported dependency download")
		}
		if !domain.SafeRelativePath(depRoot) {
			return renderError()
		}
		child, err := loadChart(files, depRoot)
		if err != nil {
			return err
		}
		if err = loadLocalDependencies(child, files, depRoot, depth+1); err != nil {
			return err
		}
		ch.AddDependency(child)
	}
	return nil
}

func hasRemoteSchema(ch *chart.Chart) bool {
	if len(ch.Schema) > 0 {
		var schema any
		if json.Unmarshal(ch.Schema, &schema) != nil {
			return true
		}
		var walk func(any) bool
		walk = func(v any) bool {
			switch x := v.(type) {
			case map[string]any:
				for k, v := range x {
					if k == "$ref" {
						s, _ := v.(string)
						if s != "" && !strings.HasPrefix(s, "#") {
							return true
						}
					}
					if walk(v) {
						return true
					}
				}
			case []any:
				for _, v := range x {
					if walk(v) {
						return true
					}
				}
			}
			return false
		}
		if walk(schema) {
			return true
		}
	}
	for _, d := range ch.Dependencies() {
		if hasRemoteSchema(d) {
			return true
		}
	}
	return false
}

func renderError() error {
	return domain.FailReason(domain.CodeLocalRenderFailed, domain.ReasonInputInvalid, "render", "local Helm input, chart, dependency or manifest is invalid", "check the source files and Helm inputs; raw template errors are withheld to protect secret values", nil)
}

type memoryInfo struct {
	name string
	dir  bool
}

func (m memoryInfo) Name() string { return path.Base(m.name) }
func (m memoryInfo) Size() int64  { return 0 }
func (m memoryInfo) Mode() fs.FileMode {
	if m.dir {
		return fs.ModeDir
	}
	return 0
}
func (m memoryInfo) ModTime() time.Time { return time.Time{} }
func (m memoryInfo) IsDir() bool        { return m.dir }
func (m memoryInfo) Sys() any           { return nil }

// sourceInputs returns the input files declared by plan source i; resolved
// lists them in plan order.
func sourceInputs(resolved []inputFile, i int) []inputFile {
	var out []inputFile
	for _, in := range resolved {
		if in.Source == i {
			out = append(out, in)
		}
	}
	return out
}
