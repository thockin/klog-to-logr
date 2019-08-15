package fixer

import (
	"go/token"

	"golang.org/x/tools/go/packages"
)

func AddErrorFrom(msg string, pos token.Pos, pkg *packages.Package) {
	pkg.Errors = append(pkg.Errors, packages.Error{
		Pos:  pkg.Fset.Position(pos).String(),
		Msg:  msg,
		Kind: packages.UnknownError,
	})

}


