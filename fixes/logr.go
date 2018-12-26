// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fixes

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"golang.org/x/tools/go/ast/astutil"
	"github.com/thockin/klog-to-logr/fixer"
	"github.com/thockin/klog-to-logr/importer"
)

const StandardKlogPkg = "k8s.io/klog"

var (
	errIdent = ast.NewIdent("err")

	// errDecl is used to as a fake declaration to load the error interface correctly
	errDecl = &ast.GenDecl{
		Tok: token.VAR,
		Specs: []ast.Spec{&ast.ValueSpec{
			Names: []*ast.Ident{errIdent},
			Type: ast.NewIdent("error"),
		}},
	}

	errFile = &ast.File{
		Name: ast.NewIdent("internal"),
		Decls: []ast.Decl{errDecl},
	}
)

// loadErrorType discovers the error type's interface for later use,
// using the typechecker and a fake file.
func loadErrorType() (*types.Interface, error) {
	// set up our configurations (no need for an importer, etc)
	checkConfig := &types.Config{}
	typeInfo := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
	}
	// load the error type's information into the above info
	_, err := checkConfig.Check("<stdin>", token.NewFileSet(), []*ast.File{errFile}, typeInfo)
	if err != nil {
	//	log.Error(err, "unable to typecheck basic error declaration for determining error type")
		return nil, err
	}
	errorObj := typeInfo.ObjectOf(errIdent)
	if errorObj == nil {
		// log.Error(err, "unable to fetch type info for error type")
		return nil, err
	}
	// the error type will be a types.Named with an underlying interface
	return errorObj.Type().Underlying().(*types.Interface), nil
}

// LogrFix returns a fixer.Fix that converts calls to klog to logr structured logging.
func LogrFix(klogPkg string) (fixer.Fix, error) {
	res := &logrFixMaker{
		klogPkg: klogPkg,
	}

	var err error
	res.errorInterface, err = loadErrorType()
	if err != nil {
		return fixer.Fix{}, err
	}

	return fixer.Fix{
		Name: "logr",
		Execute: res.fix,
		Description: `Converts klog calls to logr calls`,
	}, nil
}

// logrFixMaker produces individual instaces of logrFixes.  It carries common
// configuration (like the klog package).
type logrFixMaker struct {
	klogPkg string
	errorInterface *types.Interface
}

// fix constructs a logrFix, and runs it.  It implements the signature of fixer.Fix.Exectute.
func (f *logrFixMaker) fix(info fixer.FileInfo, loader importer.Loader, log logr.Logger) bool {
	fixer := &logrFix{
		info: info,
		loader: loader,
		log: log,
		logrFixMaker: f,
	}

	return fixer.fix()
}

// logrFix knows how to convert klog to logr.
type logrFix struct {
	log logr.Logger
	loader importer.Loader
	info fixer.FileInfo

	*logrFixMaker
}

// fix traverses the AST, looking for calls to klog and replacing them with logr.
func (f *logrFix) fix() bool {
	// If this file doesn't import klog, skip it.
	impSpec := getImportSpec(f.info.AST, f.klogPkg)
	if impSpec == nil {
		return false
	}

	// Find the canonical import info for the package.
	// TODO(directxman12): don't repeat this over and over
	bldpkg, err := build.Import(f.klogPkg, filepath.Dir(f.info.Name), 0)
	if err != nil {
		f.log.Error(err, "import failed", "pkg", f.klogPkg)
		return false
	}
	pkgImport := bldpkg.ImportPath

	// Get the name of the package.
	pkgName := bldpkg.Name   // Self-defined
	if impSpec.Name != nil { // Aliased on import
		pkgName = impSpec.Name.Name
	}
	// Rewrite the import in the AST.
	impSpec.Path = &ast.BasicLit{Kind: token.STRING, Value: `"k8s.io/client-go/log"`}

	// Process the AST and fix up calls and references.
	astutil.Apply(f.info.AST, nil, func(cursor *astutil.Cursor) bool {
		// Try statement-calls to functions in our pkg.
		f.tryPkgStmtCall(pkgName, cursor)

		// Try expression-calls to functions in our pkg.
		f.tryPkgExprCall(pkgName, cursor)

		// Try other symbols in our pkg.
		f.tryPkgSymbol(pkgName, cursor)

		// Try calls to methods on types in our pkg.
		f.tryTypedCall(pkgImport, cursor)

		return true
	})

	return true
}

func isPkgIdent(pkg string, id *ast.Ident) bool {
	// To convince us that this is the package we want:
	// 1) The anchor of this call-site (the `lhs`)
	//    must match the package name AND ...
	// 2) The identifier must have no associated Object
	if id.Name == pkg && id.Obj == nil {
		return true
	}
	return false
}

func (f *logrFix) tryPkgStmtCall(pkgName string, cursor *astutil.Cursor) bool {
	// We're looking for expression-statements (so we can add new statements if
	// needed)...
	stmt, ok := cursor.Node().(*ast.ExprStmt)
	if !ok {
		return false
	}

	// ... which are call expressions...
	callexpr, ok := stmt.X.(*ast.CallExpr)
	if !ok {
		return false
	}

	// ... which are selector expressions (e.g. `lhs.rhs()`)...
	selexpr, ok := callexpr.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	// ... which anchor on simple identifiers.
	id, ok := selexpr.X.(*ast.Ident)
	if !ok {
		return false
	}

	if !isPkgIdent(pkgName, id) {
		return false
	}

	f.log.V(5).Info("found a package stmt-call", "func", selexpr.Sel.Name)

	// We need to handle these in statement-context so we can add statements.
	// It's better to handle as much as possible as expr-calls.
	switch selexpr.Sel.Name {
	case "Fatal", "Fatalf", "Fatalln":
		f.fixError(selexpr, callexpr)
		cursor.InsertAfter(&ast.ExprStmt{
			X: &ast.CallExpr{
				Fun: &ast.SelectorExpr{
					X:   newIdent("os", 0),
					Sel: newIdent("Exit", 0),
				},
				Args: []ast.Expr{
					&ast.BasicLit{
						Kind:  token.INT,
						Value: "255",
					},
				},
			},
		})
	case "InitFlags":
		fixInitFlags(selexpr)
	default:
		return false
	}

	// Rewrite the package name.
	selexpr.X = newIdent("log", selexpr.X.Pos())

	return true
}

func (f *logrFix) tryPkgExprCall(pkgName string, cursor *astutil.Cursor) bool {
	// We're looking for call expressions...
	callexpr, ok := cursor.Node().(*ast.CallExpr)
	if !ok {
		return false
	}

	// ... which are selector expressions (e.g. `lhs.rhs()`)...
	selexpr, ok := callexpr.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	// ... which anchor on simple identifiers.
	id, ok := selexpr.X.(*ast.Ident)
	if !ok {
		return false
	}

	if !isPkgIdent(pkgName, id) {
		return false
	}

	f.log.V(5).Info("found a package expr-call", "func", selexpr.Sel.Name)

	// All of these could be embedded in larger expressions.
	switch selexpr.Sel.Name {
	case "Info", "Infof", "Infoln", "Warning", "Warningf", "Warningln":
		fixInfo(selexpr, callexpr)
	case "Error", "Errorf", "Errorln":
		f.fixError(selexpr, callexpr)
	case "V":
		// Nothing to do here, just the package name below.
	default:
		return false
	}

	// Rewrite the package name.
	selexpr.X = newIdent("log", selexpr.X.Pos())

	return true
}

func (f *logrFix) tryPkgSymbol(pkgName string, cursor *astutil.Cursor) bool {
	// We're looking for selector expressions...
	selexpr, ok := cursor.Node().(*ast.SelectorExpr)
	if !ok {
		return false
	}

	// ... which anchor on simple identifiers...
	id, ok := selexpr.X.(*ast.Ident)
	if !ok {
		return false
	}

	// To convince us that this is the package we want:
	// 1) The anchor of this call-site (the `lhs`)
	//    must match the package name AND ...
	// 2) The identifier must have no associated Object
	if id.Name != pkgName || id.Obj != nil {
		return false
	}

	f.log.V(5).Info("found a package symbol", "sym", selexpr.Sel.Name)

	switch selexpr.Sel.Name {
	case "Level":
		// Nothing to do here.  This conversion will fail because logr doesn't
		// have an equivalent, but that is OK.  We need a human to look at this.
	default:
		return false
	}

	// Rewrite the package name.
	selexpr.X = newIdent("log", selexpr.X.Pos())

	return true
}

func (f *logrFix) tryTypedCall(pkgImport string, cursor *astutil.Cursor) bool {
	// We're looking for call expressions...
	callexpr, ok := cursor.Node().(*ast.CallExpr)
	if !ok {
		return false
	}

	// ... which are selector expressions (e.g. `lhs.rhs()`)...
	selexpr, ok := callexpr.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	t := f.loader.TypeInfo().Types[selexpr.X].Type
	if t == nil {
		return false
	}
	dot := strings.LastIndexByte(t.String(), '.')
	if dot < 0 {
		return false
	}
	tp := t.String()[:dot]
	tt := t.String()[dot+1:]
	if tp == "" || tt == "" {
		f.log.Error(nil, "invalid type string", "pkg", tp, "type", tt)
		return false
	}
	if tp != pkgImport {
		// Not for us.
		return false
	}

	f.log.V(5).Info("found a method call", "type", tt)

	switch tt {
	case "Verbose":
		switch selexpr.Sel.Name {
		case "Info", "Infof", "Infoln":
			fixInfo(selexpr, callexpr)
		default:
			f.log.Error(nil, "unhandled method on Verbose", "method", selexpr.Sel.Name)
			return false
		}
	case "Level":
		//FIXME: anything to do here?
	default:
		return false
	}

	return true
}

func newIdent(name string, pos token.Pos) *ast.Ident {
	id := ast.NewIdent(name)
	id.NamePos = pos
	return id
}

func fixInfo(selexpr *ast.SelectorExpr, callexpr *ast.CallExpr) {
	selexpr.Sel = newIdent("Info", selexpr.Sel.Pos())

	newArgs := []ast.Expr{getFormatString(callexpr.Args)}
	// Generate the key-value args.
	for i, arg := range callexpr.Args {
		if i == 0 {
			continue
		}
		key := `"FIXME__unknown_key"`
		if ident, ok := arg.(*ast.Ident); ok {
			key = `"` + ident.Name + `"`
		}
		newArgs = append(newArgs, &ast.BasicLit{Kind: token.STRING, Value: key}, arg)
	}
	callexpr.Args = newArgs
}

func (f *logrFix) fixError(selexpr *ast.SelectorExpr, callexpr *ast.CallExpr) {
	selexpr.Sel = newIdent("Error", selexpr.Sel.Pos())

	// Look for the best arg to use as the error.
	isErrorType := []int{}
	isNamedErr := -1
	for i, arg := range callexpr.Args {
		t := f.loader.TypeInfo().Types[arg].Type
		f.log.V(5).Info("arg", "idx", i, "type", t.String())
		if types.Implements(t, f.errorInterface) {
			isErrorType = append(isErrorType, i)
		}
	}
	errIndex := -1
	if len(isErrorType) != 0 {
		if len(isErrorType) > 1 {
			//FIXME: print file and line
			fmt.Fprintf(os.Stderr, "WARNING: more than one argument has type `error`\n")
		}
		errIndex = isErrorType[0]
	} else if isNamedErr >= 0 {
		errIndex = isNamedErr
	}
	errExpr := "FIXME__unknown_error_expr"
	if errIndex >= 0 {
		// Remember the expression to emit later and remove it from the args list.
		errExpr = types.ExprString(callexpr.Args[errIndex])
		callexpr.Args = append(callexpr.Args[:errIndex], callexpr.Args[errIndex+1:]...)
	}

	newArgs := []ast.Expr{ast.NewIdent(errExpr), getFormatString(callexpr.Args)}
	// Generate the key-value args.
	for i, arg := range callexpr.Args {
		if i == 0 {
			continue
		}
		key := `"FIXME__unknown_key"`
		if ident, ok := arg.(*ast.Ident); ok {
			key = `"` + ident.Name + `"`
		}
		newArgs = append(newArgs, &ast.BasicLit{Kind: token.STRING, Value: key}, arg)
	}
	callexpr.Args = newArgs
}

func fixInitFlags(selexpr *ast.SelectorExpr) {
	// This will break, which is what we want.  A human needs to look at this.
	selexpr.Sel = newIdent("FIXME__InitFlags_is_not_supported", selexpr.Sel.Pos())
}

func getFormatString(args []ast.Expr) *ast.BasicLit {
	if len(args) == 0 {
		panic("No call arguments found")
	}
	lit, ok := args[0].(*ast.BasicLit)
	if !ok {
		panic("First call argument is not a literal")
	}
	if lit.Kind != token.STRING {
		panic("First call argument is not a string")
	}
	return lit
}

func getImportSpec(file *ast.File, pkg string) *ast.ImportSpec {
	for _, s := range file.Imports {
		if importPath(s) == pkg {
			return s
		}
	}
	return nil
}

// importPath returns the unquoted import path of s,
// or "" if the path is not properly quoted.
func importPath(s *ast.ImportSpec) string {
	t, err := strconv.Unquote(s.Path.Value)
	if err == nil {
		return t
	}
	return ""
}
