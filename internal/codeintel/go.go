// Package codeintel separates syntax facts (Go AST) from LSP semantic queries.
package codeintel

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"path/filepath"
)

type Symbol struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Line    int    `json:"line"`
	EndLine int    `json:"end_line"`
}
type Diagnostic struct {
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}
type Analysis struct {
	Engine      string       `json:"engine"`
	Symbols     []Symbol     `json:"symbols"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// AnalyzeGo returns syntax diagnostics even for malformed files. It does not
// claim to type-check packages, resolve references, or validate build tags.
func AnalyzeGo(path string, source []byte) (Analysis, error) {
	out := Analysis{Engine: "go/ast (syntax only)", Symbols: []Symbol{}, Diagnostics: []Diagnostic{}}
	if filepath.Ext(path) != ".go" {
		return out, fmt.Errorf("AST currently supports .go files only")
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, source, parser.AllErrors)
	if err != nil {
		if list, ok := err.(scanner.ErrorList); ok {
			for _, e := range list {
				out.Diagnostics = append(out.Diagnostics, Diagnostic{Line: e.Pos.Line, Column: e.Pos.Column, Message: e.Msg, Severity: "error"})
			}
		} else {
			return out, err
		}
	}
	if f == nil {
		return out, nil
	}
	add := func(name, kind string, n ast.Node) {
		out.Symbols = append(out.Symbols, Symbol{Name: name, Kind: kind, Line: fset.Position(n.Pos()).Line, EndLine: fset.Position(n.End()).Line})
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			kind := "function"
			if d.Recv != nil {
				kind = "method"
			}
			add(d.Name.Name, kind, d)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					add(spec.Name.Name, "type", spec)
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						add(name.Name, d.Tok.String(), spec)
					}
				}
			}
		}
	}
	return out, nil
}
