package domain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Every issue and reason code constant must be named in docs/reference.md,
// so a reader of a report can look a code up in one place.
func TestEveryCodeIsDocumented(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "codes.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			for _, value := range spec.(*ast.ValueSpec).Values {
				code, err := strconv.Unquote(value.(*ast.BasicLit).Value)
				if err != nil {
					t.Fatal(err)
				}
				codes = append(codes, code)
			}
		}
	}
	if len(codes) < 50 {
		t.Fatalf("found %d codes in codes.go; the parser missed constants", len(codes))
	}
	// Only the reference counts: a code named in an ADR or a feature page
	// alone cannot be looked up in the issue code catalog.
	docs, err := os.ReadFile("../../docs/reference.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range codes {
		if !strings.Contains(string(docs), "`"+code+"`") {
			t.Errorf("code %q is not named in docs/reference.md (add it to the issue or reason code table)", code)
		}
	}
}
