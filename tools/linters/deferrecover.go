package linters

import (
	"go/ast"
	"go/types"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
)

const (
	deferRecoverLinterName = "deferrecover"
	fnPackagePath          = "github.com/lightningnetwork/lnd/fn/v2"
)

// NewDeferRecover creates a linter plugin that verifies RecoverPanic is
// deferred directly.
func NewDeferRecover(_ any) (register.LinterPlugin, error) {
	return &DeferRecoverPlugin{}, nil
}

// DeferRecoverPlugin checks that calls to fn.RecoverPanic are direct defer
// expressions.
type DeferRecoverPlugin struct{}

// BuildAnalyzers creates the analyzers for the deferrecover linter.
//
// NOTE: This is part of the register.LinterPlugin interface.
func (d *DeferRecoverPlugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{newDeferRecoverAnalyzer()}, nil
}

// GetLoadMode returns the load mode for the deferrecover linter.
//
// NOTE: This is part of the register.LinterPlugin interface.
func (d *DeferRecoverPlugin) GetLoadMode() string {
	return register.LoadModeTypesInfo
}

// newDeferRecoverAnalyzer constructs the analyzer used by the custom linter.
func newDeferRecoverAnalyzer() *analysis.Analyzer {
	return &analysis.Analyzer{
		Name: deferRecoverLinterName,
		Doc:  "Reports fn.RecoverPanic calls that are not deferred directly",
		Run:  runDeferRecover,
	}
}

// runDeferRecover reports RecoverPanic calls that are not direct defer
// expressions.
func runDeferRecover(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		directDefers := make(map[*ast.CallExpr]struct{})
		ast.Inspect(file, func(node ast.Node) bool {
			deferStmt, ok := node.(*ast.DeferStmt)
			if ok {
				directDefers[deferStmt.Call] = struct{}{}
			}

			return true
		})

		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !isRecoverPanicCall(pass, call) {
				return true
			}

			if _, ok := directDefers[call]; ok {
				return true
			}

			pass.Reportf(
				call.Pos(), "fn.RecoverPanic only works when "+
					"deferred directly",
			)

			return true
		})
	}

	return nil, nil
}

// isRecoverPanicCall reports whether call resolves to fn.RecoverPanic.
func isRecoverPanicCall(pass *analysis.Pass, call *ast.CallExpr) bool {
	var object types.Object

	switch fn := unparen(call.Fun).(type) {
	case *ast.Ident:
		object = pass.TypesInfo.Uses[fn]

	case *ast.SelectorExpr:
		object = pass.TypesInfo.Uses[fn.Sel]
	}

	function, ok := object.(*types.Func)
	if !ok || function.Pkg() == nil {
		return false
	}

	return function.Pkg().Path() == fnPackagePath &&
		function.Name() == "RecoverPanic"
}

// unparen removes any parentheses surrounding an expression.
func unparen(expr ast.Expr) ast.Expr {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			return expr
		}

		expr = paren.X
	}
}

func init() {
	register.Plugin(deferRecoverLinterName, NewDeferRecover)
}
