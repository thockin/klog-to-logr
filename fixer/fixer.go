package fixer

import (
	"go/ast"
	"go/parser"
	"path/filepath"

	"github.com/go-logr/logr"
	"github.com/thockin/klog-to-logr/importer"
)

// FileInfo represents the information needed to process some file
// to apply a fix.
type FileInfo struct {
	Name string
	Package *importer.PackageInfo
	AST *ast.File
}

// Fix represents some fix to be applied to a given file.
type Fix struct {
	Name string
	Execute func(FileInfo, importer.Loader, logr.Logger) bool
	Description string
}

// Fixer executes fixes against packages
type Fixer struct {
	Log logr.Logger
	Fixes []Fix
	Loader importer.Loader

	HandleFix func(FileInfo) error
}

func (f *Fixer) FixPackage(pkg *importer.PackageInfo) error {
	pkgLog := f.Log.WithValues("package", pkg.BuildInfo.ImportPath)
	pkgLog.V(1).Info("applying fixes", "file count", len(pkg.Files))

	for i, ast := range pkg.Files {
		filename := pkg.BuildInfo.GoFiles[i]
		fileLog := pkgLog.WithValues("file", filename)
		info := FileInfo{
			Name: filepath.Join(pkg.BuildInfo.Dir, filename),
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
		if fix.Execute(info, f.Loader, log.WithValues("fix", fix.Name)) {
			fixed = true

			// The AST changed, so we must re-parse it for the next fix to be
			// additive.  We don't need to track the resultant ast.File beyond
			// this function because the whole universe will be torn down in the
			// outer loop calling this (for each top-level arg).
			newSrc, err := GofmtFile(info.AST, f.Loader.FileSet())
			if err != nil {
				return err
			}
			info.AST, err = parser.ParseFile(f.Loader.FileSet(), info.Name, newSrc, parser.ParseComments)
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
