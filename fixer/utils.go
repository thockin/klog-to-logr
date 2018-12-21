package fixer

import (
	"go/ast"
	"go/format"
	"go/token"
	"bytes"
)

// gofmtFile formats the given file that's part of the given fileset,
// returning the results
func GofmtFile(f *ast.File, fset *token.FileSet) ([]byte, error) {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
