// Package wiredoc reads the wire package's doc comments from its source, for
// wiregen. Separate from wire so the plugin binary never links a Go parser.
package wiredoc

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"

	"github.com/branchkit/branchkit-plugin-browser/wire"
)

// Read collects type and field doc comments from the wire package's source
// directory, for the generated TypeScript.
func Read(dir string) (wire.Docs, error) {
	d := wire.Docs{Types: map[string]string{}, Fields: map[string]string{}}
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return d, err
	}
	if len(files) == 0 {
		return d, fmt.Errorf("no Go files in %s (run from plugins/browser/src, or pass -src)", dir)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return d, err
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				doc := ts.Doc
				if doc == nil && len(gd.Specs) == 1 {
					doc = gd.Doc
				}
				d.Types[ts.Name.Name] = text(doc)
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range st.Fields.List {
					c := text(field.Doc)
					if c == "" {
						c = text(field.Comment)
					}
					for _, n := range field.Names {
						d.Fields[ts.Name.Name+"."+n.Name] = c
					}
				}
			}
		}
	}
	return d, nil
}

func text(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	return strings.TrimSpace(g.Text())
}
