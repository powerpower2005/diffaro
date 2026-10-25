package profile

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// maxYAMLProblems bounds the problems one decode error names.
const maxYAMLProblems = 5

var (
	unknownField   = regexp.MustCompile(`^line (\d+): field (.+) not found in type \S+$`)
	duplicateKey   = regexp.MustCompile(`^line (\d+): mapping key "(.*)" already defined at line (\d+)$`)
	wrongKind      = regexp.MustCompile(`^line (\d+): cannot unmarshal !!(\w+)(?: .*)? into (\S+)$`)
	syntaxProblem  = regexp.MustCompile(`^yaml: line (\d+): (.+)$`)
	parserPhrase   = regexp.MustCompile(`^[a-z ,'\[\]{}:-]+$`)
	displayableKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,39}$`)
	digit          = regexp.MustCompile(`[0-9]`)
)

// parserProblems are the messages of yaml.v3 parser errors, whose line is
// one less than the line in the file. Scanner errors never use them.
var parserProblems = map[string]bool{
	"did not find expected <stream-start>":   true,
	"did not find expected <document start>": true,
	"found undefined tag handle":             true,
	"did not find expected node content":     true,
	"did not find expected '-' indicator":    true,
	"did not find expected key":              true,
	"did not find expected ',' or ']'":       true,
	"did not find expected ',' or '}'":       true,
	"found duplicate %YAML directive":        true,
	"found incompatible YAML document":       true,
	"found duplicate %TAG directive":         true,
}

// yamlProblems states where a profile fails to decode: line numbers, key
// names, YAML kinds and fixed parser phrases. Decoder text can
// quote values, and a profile can hold tokens, so nothing else is kept. A
// key that could be a credential, such as one with many digits, is
// withheld.
func yamlProblems(err error) []string {
	var lines []string
	var typeError *yaml.TypeError
	if errors.As(err, &typeError) {
		lines = typeError.Errors
	} else {
		lines = []string{err.Error()}
	}
	var problems []string
	for _, line := range lines {
		if len(problems) == maxYAMLProblems {
			problems = append(problems, "more problems follow")
			break
		}
		if problem := yamlProblem(strings.TrimSpace(line)); problem != "" {
			problems = append(problems, problem)
		}
	}
	return problems
}

func yamlProblem(line string) string {
	if m := unknownField.FindStringSubmatch(line); m != nil {
		return "line " + m[1] + ": unknown field " + key(m[2])
	}
	if m := duplicateKey.FindStringSubmatch(line); m != nil {
		return "line " + m[1] + ": key " + key(m[2]) + " is already defined at line " + m[3]
	}
	if m := wrongKind.FindStringSubmatch(line); m != nil {
		return "line " + m[1] + ": a " + yamlKind(m[2]) + " is not valid here; expected " + expectedKind(m[3])
	}
	if m := syntaxProblem.FindStringSubmatch(line); m != nil {
		number := m[1]
		if parserProblems[m[2]] {
			// yaml.v3 counts the lines of parser errors from zero and those
			// of scanner errors from one.
			n, _ := strconv.Atoi(number)
			number = strconv.Itoa(n + 1)
		}
		if parserPhrase.MatchString(m[2]) {
			return "line " + number + ": " + m[2]
		}
		return "line " + number + ": invalid YAML syntax"
	}
	return ""
}

func key(name string) string {
	if !displayableKey.MatchString(name) || len(digit.FindAllString(name, -1)) > 4 {
		return "(name withheld)"
	}
	return name
}

func yamlKind(tag string) string {
	switch tag {
	case "str":
		return "string"
	case "int", "float":
		return "number"
	case "bool":
		return "boolean"
	case "seq":
		return "list"
	case "map":
		return "mapping"
	case "null":
		return "null value"
	}
	return "value"
}

// expectedKind names the YAML shape of a Go target type.
func expectedKind(goType string) string {
	switch {
	case strings.HasPrefix(goType, "[]"):
		return "a list"
	case strings.HasPrefix(goType, "map["), strings.Contains(goType, "."):
		return "a mapping"
	case goType == "string":
		return "a string"
	case goType == "bool":
		return "true or false"
	case strings.HasPrefix(goType, "int"), strings.HasPrefix(goType, "uint"), strings.HasPrefix(goType, "float"):
		return "a number"
	}
	return "another value"
}
