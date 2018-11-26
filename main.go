// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Hacked for klog/glog -> logr

package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/go-logr/glogr"
	"github.com/go-logr/logr"
	"k8s.io/klog/glog"
)

var (
	fset     = token.NewFileSet()
	typeInfo = &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
	}
)

// FIXME: we probably don't need all this registration stuff.  Better to be a purpose-built tool.
type Fix struct {
	name string
	fn   func(string, *ast.File) bool
	desc string
}

type byName []Fix

func (f byName) Len() int           { return len(f) }
func (f byName) Swap(i, j int)      { f[i], f[j] = f[j], f[i] }
func (f byName) Less(i, j int) bool { return f[i].name < f[j].name }

var allFixes []Fix

func register(f Fix) {
	allFixes = append(allFixes, f)
}

var doDiff = flag.Bool("diff", false, "print diffs instead of rewriting files")

func usage() {
	fmt.Fprintf(os.Stderr, "usage: kfix [-diff] [path ...]\n")
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\nAvailable fixups are:\n")
	sort.Sort(byName(allFixes))
	for _, f := range allFixes {
		fmt.Fprintf(os.Stderr, "\n%s\n", f.name)
		desc := strings.TrimSpace(f.desc)
		desc = strings.Replace(desc, "\n", "\n\t", -1)
		fmt.Fprintf(os.Stderr, "\t%s\n", desc)
	}
	os.Exit(93)
}

type Package struct {
	Name      string
	ASTFiles  []*ast.File
	Filenames []string
}

// Global logger.
var log logr.Logger

func main() {
	flag.Usage = usage
	flag.Parse()

	log = glogr.New()
	defer glog.Flush()

	if flag.NArg() == 0 {
		usage()
	}

	//FIXME: suport foo.com/repo/pkg/... syntax
	for i := 0; i < flag.NArg(); i++ {
		arg := flag.Arg(i)
		bldpkg, err := build.Import(arg, ".", 0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "can't fix %q: %v\n", arg, err)
			os.Exit(1)
		}
		pkg := &Package{Name: arg}
		conf := types.Config{Importer: importer.Default()} //FIXME: this is looking for .a dirs
		//conf := types.Config{Importer: buildImporter{}} //FIXME: fails because it didn't parse fmt
		dir, err := filepath.Abs(bldpkg.Dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "can't get absolute path for pkg-dir %q: %v\n", bldpkg.Dir, err)
			os.Exit(2)
		}
		for _, filename := range append(append([]string{}, bldpkg.GoFiles...), bldpkg.TestGoFiles...) {
			path := filepath.Join(dir, filename)
			ast, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				fmt.Fprintf(os.Stderr, "can't parse %q: %v\n", path, err)
				os.Exit(3)
			}
			pkg.ASTFiles = append(pkg.ASTFiles, ast)
			pkg.Filenames = append(pkg.Filenames, path)
		}
		_, err = conf.Check(".", fset, pkg.ASTFiles, typeInfo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "can't typecheck %q: %v\n", arg, err)
			os.Exit(4)
		}
		log.V(2).Info("processing package", "pkg", bldpkg.Dir)
		if err := doPkg(pkg); err != nil {
			fmt.Fprintf(os.Stderr, "aborting package %q: %v\n", arg, err)
			os.Exit(5)
		}
	}

	os.Exit(0)
}

func gofmtFile(f *ast.File) ([]byte, error) {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type buildImporter struct{}

func (bi buildImporter) Import(path string) (*types.Package, error) {
	return bi.ImportFrom(path, "", 0)
}
func (buildImporter) ImportFrom(path, src string, mode types.ImportMode) (*types.Package, error) {
	//FIXME: if we use this mode, save a cache for dups
	bp, err := build.Import(path, src, 0) // build.FindOnly here and other?
	if err != nil {
		return nil, err
	}
	fmt.Printf("IMPORTING: %v from %v\n", bp.ImportPath, bp.Dir)
	pkg := types.NewPackage(bp.Dir, bp.ImportPath)
	pkg.SetImports(nil) //FIXME: do I need this?
	pkg.MarkComplete()
	return pkg, nil
}

func readFile(filename string) ([]byte, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	src, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return src, nil
}

func doPkg(pkg *Package) error {
	for i, _ := range pkg.ASTFiles {
		filename := pkg.Filenames[i]
		ast := pkg.ASTFiles[i]
		if err := doFile(filename, ast); err != nil {
			return err
		}
	}
	return nil
}

func doFile(filename string, ast *ast.File) error {
	// Get the original source.
	src, err := readFile(filename)
	if err != nil {
		return err
	}

	// Apply fixes to this file.
	fixed := false
	for _, fix := range allFixes {
		if fix.fn(filename, ast) {
			fixed = true

			// The AST changed, so we must re-parse it for the next fix to be
			// additive.  We don't need to track the resultant ast.File beyond
			// this function because the whole universe will be torn down in the
			// outer loop calling this (for each top-level arg).
			newSrc, err := gofmtFile(ast)
			if err != nil {
				return err
			}
			ast, err = parser.ParseFile(fset, filename, newSrc, parser.ParseComments)
			if err != nil {
				return err
			}
		}
	}
	if !fixed {
		return nil
	}

	// Format the AST again.  We did this after each fix, so it appears
	// redundant, but it is necessary to generate gofmt-compatible
	// source code in a few cases. The official gofmt style is the
	// output of the printer run on a standard AST generated by the parser,
	// but the source we generated inside the loop above is the
	// output of the printer run on a mangled AST generated by a fixer.
	newSrc, err := gofmtFile(ast)
	if err != nil {
		return err
	}

	if *doDiff {
		data, err := diff(src, newSrc)
		if err != nil {
			return fmt.Errorf("computing diff: %s", err)
		}
		fmt.Printf("diff %s %s\n", filename, filepath.Join("fixed", filename))
		os.Stdout.Write(data)
		return nil
	}

	return ioutil.WriteFile(filename, newSrc, 0)
}

var gofmtBuf bytes.Buffer

func gofmt(n interface{}) string {
	gofmtBuf.Reset()
	if err := format.Node(&gofmtBuf, fset, n); err != nil {
		return "<" + err.Error() + ">"
	}
	return gofmtBuf.String()
}

func writeTempFile(dir, prefix string, data []byte) (string, error) {
	file, err := ioutil.TempFile(dir, prefix)
	if err != nil {
		return "", err
	}
	_, err = file.Write(data)
	if err1 := file.Close(); err == nil {
		err = err1
	}
	if err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

func diff(b1, b2 []byte) (data []byte, err error) {
	f1, err := writeTempFile("", "kfix", b1)
	if err != nil {
		return
	}
	defer os.Remove(f1)

	f2, err := writeTempFile("", "kfix", b2)
	if err != nil {
		return
	}
	defer os.Remove(f2)

	cmd := "diff"
	if runtime.GOOS == "plan9" {
		cmd = "/bin/ape/diff"
	}

	data, err = exec.Command(cmd, "-u", f1, f2).CombinedOutput()
	if len(data) > 0 {
		// diff exits with a non-zero status when the files don't match.
		// Ignore that failure as long as we get output.
		err = nil
	}
	return
}
