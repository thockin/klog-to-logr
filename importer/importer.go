package importer

import (
	"fmt"
	"sync"
	"go/types"
	"go/build"
	"go/token"
	"go/parser"
	"go/ast"
	"go/importer"
	pathpkg "path"
	"path/filepath"

	"github.com/go-logr/logr"
)

// TODO(directxman12): some day, I'll put this in a library so that
// I don't just copy it from project to project...

type Loader interface {
	// PackageInfoFor returns the package information for the given package,
	// without any explicit source directory.
	PackageInfoFor(path string) *PackageInfo
}

// pkgIdent contains identifying information for a particular import
type pkgIdent struct {
	path string
}

// PackageInfo represents loading and typechecking information for a
// particular package.
type PackageInfo struct {
	ParseInfo *types.Package
	BuildInfo *build.Package
	Files []*ast.File
}

func (i pkgIdent) String() string {
	return fmt.Sprintf("%q", i.path)
}

// KindaFastImporter is a types.ImporterFrom that uses a "fast" importer that
// reads pre-compiled intermediate code, then falls back to a "slower" from-source
// method if that fails.
type KindaFastImporter struct {
	// fastImporter is a types.ImporterFrom that's expected to be relatively quick
	fastImporter types.ImporterFrom
	// cwd is the current working directory
	cwd string
	// packages represents a map of loaded packages, to avoid loops and reprocessing.
	// a nil-but-present entry implies that a package is being loaded, so a loop was found.
	packages map[pkgIdent]*PackageInfo
	// config is the typechecking config used to typecheck packages.
	config *types.Config
	// fileSet is the compiler fileSet used to track parsing errors
	fileSet *token.FileSet

	// log is how we log information about what we're doing
	log logr.Logger
}

func NewImporter(cwd string, log logr.Logger) (types.ImporterFrom, Loader) {
	i := &KindaFastImporter{
		fastImporter: importer.Default().(types.ImporterFrom),
		cwd: cwd,
		packages: make(map[pkgIdent]*PackageInfo),
		fileSet: token.NewFileSet(),
		log: log,
	}
	// config references the importer, so initialize it after
	i.config = &types.Config{
		FakeImportC:      true,
		Importer:         i,
	}

	return i, i
}

func (i *KindaFastImporter) Import(path string) (*types.Package, error) {
	return i.ImportFrom(path, "", 0)
}

func (i *KindaFastImporter) ImportFrom(path, srcDir string, mode types.ImportMode) (*types.Package, error) {
	log := i.log.WithValues("import path", path, "source directory", srcDir)
	log.V(1).Info("trying to import package")

	// first, double-check that we haven't already loaded this
	ident := pkgIdent{path: path}
	if pkg, present := i.packages[ident]; present {
		if pkg == nil {
			return nil, fmt.Errorf("import loop detected for package %s", ident)
		}
		log.V(1).Info("package already imported")
		return pkg.ParseInfo, nil
	}

	// mark that we're checking to avoid loops
	i.packages[ident] = nil

	// next, try fast import:
	// figure out where srcDir actually is...
	fullDir := pathpkg.Join(i.cwd, srcDir)
	// ...and fast-import from it
	pkg, err := i.fastImporter.ImportFrom(path, fullDir, mode)
	if err == nil {
		// yay, fast import!
		log.V(1).Info("fast-imported package")
		buildPkg, err := build.Default.Import(path, fullDir, build.AllowBinary | build.FindOnly)
		if err != nil {
			return nil, err
		}
		i.packages[ident] = &PackageInfo{
			BuildInfo: buildPkg,
			ParseInfo: pkg,
		}
		return pkg, nil
	}
	
	// otherwise, import the package from source
	log.V(1).Info("unable to fast-import package, importing from source", "error", err)

	// import, grabbing the build information
	buildPkg, err := build.Default.Import(path, fullDir, build.AllowBinary)
	if err != nil {
		return nil, err
	}

	// TODO(directxman12): support xtest and test files
	// parse the files in the package...
	files, err := i.parsePackageFiles(buildPkg)
	if err != nil {
		return nil, err
	}
	// ...and check them to get a package
	pkg, err = i.config.Check(path, i.fileSet, files, nil)
	if err != nil {
		return nil, err
	}
	i.packages[ident] = &PackageInfo{
		BuildInfo: buildPkg,
		ParseInfo: pkg,
		Files: files,
	}
	log.V(1).Info("imported package from source")

	return pkg, nil
}

func (i *KindaFastImporter) parsePackageFiles(buildPkg *build.Package) ([]*ast.File, error) {
	// TODO(directxman12): cgo files, test files, xtest files
	files := make([]*ast.File, len(buildPkg.GoFiles))
	errors := make([]error, len(buildPkg.GoFiles))

	var wg sync.WaitGroup
	for ind, filename := range buildPkg.GoFiles {
		wg.Add(1)
		go func(ind int, filepath string) {
			defer wg.Done()
			files[ind], errors[ind] = parser.ParseFile(i.fileSet, filepath, nil, 0) // ok to access fset concurrently
		}(ind, filepath.Join(buildPkg.Dir, filename))
	}
	wg.Wait()

	// if there are errors, return the first one for deterministic results
	var firstError error
	for ind, err := range errors {
		if err != nil {
			i.log.Error(err, "unable to parse file", "path", filepath.Join(buildPkg.Dir, buildPkg.GoFiles[ind]))
			if firstError == nil {
				firstError = err
			}
		}
	}

	return files, firstError
}

func (i *KindaFastImporter) PackageInfoFor(path string) *PackageInfo {
	return i.packages[pkgIdent{path: path}]
}
