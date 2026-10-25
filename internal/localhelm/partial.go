package localhelm

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"

	"diffaro/internal/domain"
	"diffaro/internal/helmerror"
	"github.com/Masterminds/sprig/v3"
	"helm.sh/helm/v4/pkg/chart/common"
	chart "helm.sh/helm/v4/pkg/chart/v2"
)

// Only structural parse diagnostics are retained. Execution errors can contain
// values (including secrets), so their raw messages never cross this boundary.
var missingFunction = regexp.MustCompile(`^(?:Error: )?parse error at \(([A-Za-z0-9_./-]+):([0-9]+)\): function "([A-Za-z_][A-Za-z0-9_]{0,127})" not defined`)
var sharedTemplateDefinition = regexp.MustCompile(`{{-?\s*(define|block)\b`)
var actionWord = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

type helmParseFailure struct {
	diagnostic string
	cause      error
}

func (e *helmParseFailure) Error() string { return e.cause.Error() }
func (e *helmParseFailure) Unwrap() error { return e.cause }

func partial(err error) bool {
	var p *domain.RenderPartial
	return errors.As(err, &p)
}

func templateFailure(ch *chart.Chart, raw string) error {
	m := missingFunction.FindStringSubmatch(raw)
	if len(m) != 4 || !domain.SafeRelativePath(m[1]) {
		return templateExecutionFailure(ch, raw)
	}
	return domain.Fail(domain.CodeHelmFunctionUnavailable, "render", fmt.Sprintf("template %s:%s uses unavailable function %q", m[1], m[2], m[3]), "use the configured Argo CD render backend or a trusted Helm executable that provides this function; incomplete targets remain unknown", nil)
}

func partialFailure(reasons []string) error {
	return &domain.RenderPartial{Cause: domain.Fail(domain.CodeRenderPartial, "render", "incomplete Helm targets: "+strings.Join(reasons, "; "), "restore the required custom Helm functions or select the configured Argo CD render backend; missing targets and descendant discovery remain unknown", nil)}
}

// recoverTemplate removes only a root resource template with no shared effects.
// Helpers, subcharts, dynamic evaluation and mutable custom-function arguments
// cannot be isolated safely. No function is stubbed or given invented values.
// index carries the parsed templates of ch from one recovery to the next of
// the same render.
func recoverTemplate(ch *chart.Chart, raw string, index *templateIndex) (string, bool) {
	m := missingFunction.FindStringSubmatch(raw)
	if len(m) != 4 || !domain.SafeRelativePath(m[1]) {
		return "", false
	}
	prefix := ch.Metadata.Name + "/"
	if !strings.HasPrefix(m[1], prefix) {
		return "", false
	}
	name := strings.TrimPrefix(m[1], prefix)
	if !strings.HasPrefix(name, "templates/") || strings.HasPrefix(path.Base(name), "_") || path.Base(name) == "NOTES.txt" {
		return "", false
	}
	for i, f := range ch.Templates {
		if f.Name != name {
			continue
		}
		if !isolatedTemplate(index, ch, f, m[3]) {
			return "", false
		}
		ch.Templates = append(ch.Templates[:i], ch.Templates[i+1:]...)
		return name, true
	}
	return "", false
}

// knownFunctions are the template functions a recovered template may call
// besides the unavailable one: sprig without environment access, Go template
// builtins and Helm's own functions.
var knownFunctions = func() map[string]bool {
	known := map[string]bool{}
	for name := range sprig.TxtFuncMap() {
		known[name] = true
	}
	delete(known, "env")
	delete(known, "expandenv")
	for _, name := range strings.Fields("and or not len index slice eq ne lt le gt ge print printf println html js urlquery include required toToml mustToToml fromToml toYaml mustToYaml toYamlPretty fromYaml fromYamlArray toJson mustToJson fromJson fromJsonArray") {
		known[name] = true
	}
	return known
}()

func isolatedTemplate(index *templateIndex, ch *chart.Chart, file *common.File, unavailable string) bool {
	if sharedTemplateDefinition.Match(file.Data) {
		return false
	}
	index.use(chartTemplates(ch))
	// A resource defining named templates can affect every other template.
	candidate, err := template.New("candidate").Funcs(index.funcs).Parse(string(file.Data))
	if err != nil || len(candidate.Templates()) != 1 || index.all == nil {
		return false
	}
	w := templateWalker{index: index, unavailable: unavailable, visiting: map[string]bool{}}
	return w.walk(candidate.Tree.Root)
}

// chartTemplates lists the templates of ch and its subcharts, depth first.
func chartTemplates(ch *chart.Chart) []*common.File {
	files := append([]*common.File(nil), ch.Templates...)
	for _, d := range ch.Dependencies() {
		files = append(files, chartTemplates(d)...)
	}
	return files
}

// templateIndex is what isolating a template reads from every template of a
// chart and its subcharts: each template's action words, which parse as
// functions (funcs), and all templates parsed into one set (all), with the
// named templates defined differently by two of them (conflicting). all is
// nil when a template does not parse. Each template is named file-<position>
// in all.
type templateIndex struct {
	order       []*common.File
	words       map[*common.File][]string
	funcs       template.FuncMap
	all         *template.Template
	conflicting map[string]bool
	// positional is set when a template mentions "file-", which could name a
	// template by its position in all.
	positional bool
}

// use makes index describe files. The index of earlier templates is kept
// when files are those templates in the same order without recovered ones,
// which define nothing (isolatedTemplate), and the remaining action words are
// the same: parsing files again would then give the same result.
func (index *templateIndex) use(files []*common.File) {
	if index.order != nil && !index.positional && index.covers(files) {
		return
	}
	*index = templateIndex{words: map[*common.File][]string{}, funcs: template.FuncMap{}, order: files}
	for _, f := range files {
		var words []string
		for _, action := range templateAction.FindAll(f.Data, -1) {
			words = append(words, actionWord.FindAllString(string(action), -1)...)
		}
		index.words[f] = words
		for _, word := range words {
			index.funcs[word] = func(...any) any { return nil }
		}
		index.positional = index.positional || bytes.Contains(f.Data, []byte("file-"))
	}
	all := template.New("helpers").Funcs(index.funcs)
	definitions := map[string]string{}
	conflicting := map[string]bool{}
	for i, f := range files {
		name := fmt.Sprintf("file-%d", i)
		one, err := template.New(name).Funcs(index.funcs).Parse(string(f.Data))
		if err != nil {
			// Not kept: the next recovery parses its templates again.
			index.order = nil
			return
		}
		for _, t := range one.Templates() {
			// Adding the parsed trees is what parsing f into all would do.
			if _, err = all.AddParseTree(t.Name(), t.Tree); err != nil {
				index.order = nil
				return
			}
			if t.Name() == name {
				continue
			}
			body := t.Tree.Root.String()
			if prior, ok := definitions[t.Name()]; ok && prior != body {
				conflicting[t.Name()] = true
			}
			definitions[t.Name()] = body
		}
	}
	index.all, index.conflicting = all, conflicting
}

// covers reports whether files are indexed templates in index order and name
// the same action words as all of them.
func (index *templateIndex) covers(files []*common.File) bool {
	words := map[string]bool{}
	next := 0
	for _, f := range files {
		for next < len(index.order) && index.order[next] != f {
			next++
		}
		if next == len(index.order) {
			return false
		}
		next++
		for _, word := range index.words[f] {
			words[word] = true
		}
	}
	return len(words) == len(index.funcs)
}

// templateWalker checks that a candidate template reads only known functions,
// calls the unavailable one with literal arguments only and includes only
// named templates that do the same, within a bounded number of nodes.
type templateWalker struct {
	index       *templateIndex
	unavailable string
	visiting    map[string]bool
	count       int
}

func (w *templateWalker) helper(name string) bool {
	if w.visiting[name] || w.index.conflicting[name] {
		return false
	}
	t := w.index.all.Lookup(name)
	if t == nil || t.Tree == nil {
		return false
	}
	w.visiting[name] = true
	ok := w.walk(t.Tree.Root)
	delete(w.visiting, name)
	return ok
}

func (w *templateWalker) walk(node parse.Node) bool {
	if node == nil {
		return true
	}
	w.count++
	if w.count > 10000 {
		return false
	}
	switch n := node.(type) {
	case *parse.ListNode:
		if n != nil {
			for _, v := range n.Nodes {
				if !w.walk(v) {
					return false
				}
			}
		}
	case *parse.ActionNode:
		return w.walk(n.Pipe)
	case *parse.PipeNode:
		if len(n.Cmds) > 1 {
			for _, cmd := range n.Cmds {
				if len(cmd.Args) > 0 {
					if id, ok := cmd.Args[0].(*parse.IdentifierNode); ok && id.Ident == w.unavailable {
						return false
					}
				}
			}
		}
		for _, v := range n.Cmds {
			if !w.walk(v) {
				return false
			}
		}
	case *parse.CommandNode:
		return w.command(n)
	case *parse.IfNode:
		return w.walk(n.Pipe) && w.walk(n.List) && w.walk(n.ElseList)
	case *parse.RangeNode:
		return w.walk(n.Pipe) && w.walk(n.List) && w.walk(n.ElseList)
	case *parse.WithNode:
		return w.walk(n.Pipe) && w.walk(n.List) && w.walk(n.ElseList)
	case *parse.TemplateNode:
		return w.helper(n.Name) && w.walk(n.Pipe)
	case *parse.ChainNode:
		return w.walk(n.Node)
	}
	return true
}

func (w *templateWalker) command(n *parse.CommandNode) bool {
	if len(n.Args) > 0 {
		if id, ok := n.Args[0].(*parse.IdentifierNode); ok {
			if !knownFunctions[id.Ident] && id.Ident != w.unavailable {
				return false
			}
			switch id.Ident {
			case "set", "unset", "merge", "mergeOverwrite", "mustMerge", "mustMergeOverwrite", "tpl", "call":
				return false
			case "include":
				if len(n.Args) != 3 {
					return false
				}
				s, ok := n.Args[1].(*parse.StringNode)
				if !ok || !w.helper(s.Text) {
					return false
				}
			}
			if id.Ident == w.unavailable {
				for _, arg := range n.Args[1:] {
					switch arg.(type) {
					case *parse.StringNode, *parse.NumberNode, *parse.BoolNode:
					default:
						return false
					}
				}
			}
		}
	}
	for _, v := range n.Args {
		if !w.walk(v) {
			return false
		}
	}
	return true
}

func templateExecutionFailure(ch *chart.Chart, raw string) error {
	if failure, ok := templateLocationFailure(ch, raw); ok {
		return failure
	}
	return renderError()
}

// templateLocationFailure reports false when no safe template location
// exists. The message of a fail or required call is shown only when it is a
// literal in the chart source (chartMessage); otherwise it is withheld and
// the action says how to see it. ch may be nil.
func templateLocationFailure(ch *chart.Chart, raw string) (error, bool) {
	detail, ok := helmerror.Describe(raw)
	if !ok {
		return nil, false
	}
	if message, ok := chartMessage(ch, raw); ok {
		return domain.FailReason(domain.CodeLocalRenderFailed, domain.ReasonTemplateFailed, "render", detail+"; chart message: "+strconv.Quote(message), "follow the chart message, the chart's own check of this Application's Helm inputs at this revision", nil), true
	}
	return domain.FailReason(domain.CodeLocalRenderFailed, domain.ReasonTemplateFailed, "render", detail+"; the raw Helm message is withheld to protect values", "inspect that template line and its Helm inputs for this Application and revision; "+helmerror.ReproduceHint, nil), true
}
