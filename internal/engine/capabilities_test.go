package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capabilities.go claims to hold every optional capability, and the engine
// author's guide lists them: an interface declared anywhere else in the
// package, or missing from the guide, is one an engine author will not find.
func TestEveryCapabilityIsInCapabilitiesGoAndTheGuide(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "adding-an-engine.md"))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				if _, ok := ts.Type.(*ast.InterfaceType); !ok || ts.Name.Name == "Engine" {
					continue
				}
				found++
				if name != "capabilities.go" {
					t.Errorf("capability %s is declared in %s, not capabilities.go", ts.Name.Name, name)
				}
				if !strings.Contains(string(guide), "type "+ts.Name.Name+" interface") {
					t.Errorf("capability %s is not documented in docs/adding-an-engine.md", ts.Name.Name)
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("no capability interfaces found")
	}
}
