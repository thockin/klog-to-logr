package fixer

import (
	"go/ast"
	"go/parser"

	"github.com/go-logr/logr"
	"golang.org/x/tools/go/packages"
)

// FileInfo represents the information needed to process some file
// to apply a fix.
type FileInfo struct {
	Name string
	Package *packages.Package
	AST *ast.File
}

// Fix represents some fix to be applied to a given file.
type Fix struct {
	Name string
	Execute func(FileInfo, logr.Logger) bool
	Description string
}

// Fixer executes fixes against packages
type Fixer struct {
	Log logr.Logger
	Fixes []Fix

	HandleFix func(FileInfo) error
}

func (f *Fixer) FixPackage(pkg *packages.Package) error {
	pkgLog := f.Log.WithValues("package", pkg.PkgPath)
	pkgLog.V(1).Info("applying fixes", "file count", len(pkg.Syntax))

	for i, ast := range pkg.Syntax {
		filename := pkg.GoFiles[i]
		fileLog := pkgLog.WithValues("file", filename)
		info := FileInfo{
			Name: filename,
			AST: ast,
			Package: pkg,
		}
		if err := f.fixFile(info, fileLog); err != nil {
			return err
		}
	}

	return nil
}

func (f *Fixer) fixFile(info FileInfo, log logr.Logger) error {
	// Apply fixes to this file.
	fixed := false
	for _, fix := range f.Fixes {
		if fix.Execute(info, log.WithValues("fix", fix.Name)) {
			fixed = true

			// The AST changed, so we must re-parse it for the next fix to be
			// additive.  We don't need to track the resultant ast.File beyond
			// this function because the whole universe will be torn down in the
			// outer loop calling this (for each top-level arg).
			newSrc, err := GofmtFile(info.AST, info.Package.Fset)
			if err != nil {
				return err
			}
			info.AST, err = parser.ParseFile(info.Package.Fset, info.Name, newSrc, parser.ParseComments)
			if err != nil {
				return err
			}
		}
	}
	if !fixed {
		return nil
	}

	return f.HandleFix(info)
}
