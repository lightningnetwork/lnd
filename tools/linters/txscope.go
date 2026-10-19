package linters

import (
	"fmt"
	"go/ast"
	"go/types"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
)

const txScopeLinterName = "txscope"

// NewTxScope creates a plugin that detects access to the transaction origin
// from within an ExecTx callback.
func NewTxScope(_ any) (register.LinterPlugin, error) {
	return &TxScopePlugin{}, nil
}

// TxScopePlugin prevents transaction callbacks from acquiring another
// connection from the executor that owns the current transaction.
type TxScopePlugin struct{}

// BuildAnalyzers returns the analyzers provided by the plugin.
//
// NOTE: This is part of the register.LinterPlugin interface.
func (t *TxScopePlugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{newTxScopeAnalyzer()}, nil
}

// GetLoadMode requests type information so references to the same executor can
// be matched by object identity instead of variable spelling.
//
// NOTE: This is part of the register.LinterPlugin interface.
func (t *TxScopePlugin) GetLoadMode() string {
	return register.LoadModeTypesInfo
}

func newTxScopeAnalyzer() *analysis.Analyzer {
	return &analysis.Analyzer{
		Name: txScopeLinterName,
		Doc: "Reports access to an ExecTx transaction origin from " +
			"within its callback",
		Run: runTxScope,
	}
}

func runTxScope(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}

			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "ExecTx" {
				return true
			}

			callback := execTxCallback(pass.TypesInfo, call)
			if callback == nil {
				pass.Reportf(
					call.Pos(), "ExecTx callback must be an inline "+
						"function so transaction-origin access can "+
						"be checked",
				)

				return true
			}

			origin, ok := expressionKey(pass.TypesInfo, selector.X)
			if !ok {
				return true
			}

			checkTxCallback(pass, callback, origin)

			return true
		})
	}

	return nil, nil
}

// execTxCallback returns the inline transaction callback passed to ExecTx.
func execTxCallback(info *types.Info, call *ast.CallExpr) *ast.FuncLit {
	for _, arg := range call.Args {
		callback, ok := arg.(*ast.FuncLit)
		if !ok {
			continue
		}

		// An ExecTx callback has one transaction-query parameter. The reset
		// callback, when present, has no parameters.
		callbackType := info.TypeOf(callback)
		if callbackType == nil {
			continue
		}

		signature, ok := callbackType.Underlying().(*types.Signature)
		if ok && signature.Params().Len() == 1 {
			return callback
		}
	}

	return nil
}

func checkTxCallback(pass *analysis.Pass, callback *ast.FuncLit,
	origin string) {

	origins := map[string]struct{}{origin: {}}

	// Resolve local aliases before reporting uses. Iterate to a fixed point
	// so chains such as second := first; third := second are recognized even
	// if declarations appear in an unusual order.
	for {
		changed := collectOriginAliases(pass.TypesInfo, callback.Body, origins)
		if !changed {
			break
		}
	}

	reported := make(map[ast.Expr]struct{})
	assignmentTargets := collectAssignmentTargets(callback.Body)
	ast.Inspect(callback.Body, func(node ast.Node) bool {
		expr, ok := node.(ast.Expr)
		if !ok || !isOriginExpression(pass.TypesInfo, expr, origins) {
			return true
		}
		if _, ok := assignmentTargets[expr]; ok {
			return true
		}

		// Report only the outermost matching expression. For example, report
		// i.db rather than both i.db and a surrounding parenthesized form.
		if _, ok := reported[expr]; ok {
			return true
		}

		reported[expr] = struct{}{}
		pass.Reportf(
			expr.Pos(), "transaction origin accessed inside its "+
				"ExecTx callback; use the callback's transaction "+
				"query parameter instead",
		)

		return false
	})
}

func collectAssignmentTargets(body *ast.BlockStmt) map[ast.Expr]struct{} {
	targets := make(map[ast.Expr]struct{})
	ast.Inspect(body, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.AssignStmt:
			for _, lhs := range statement.Lhs {
				targets[lhs] = struct{}{}
			}

		case *ast.ValueSpec:
			for _, name := range statement.Names {
				targets[name] = struct{}{}
			}
		}

		return true
	})

	return targets
}

func collectOriginAliases(info *types.Info, body *ast.BlockStmt,
	origins map[string]struct{}) bool {

	changed := false
	ast.Inspect(body, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.AssignStmt:
			for index, rhs := range statement.Rhs {
				if !isOriginExpression(info, rhs, origins) ||
					index >= len(statement.Lhs) {

					continue
				}

				key, ok := expressionKey(info, statement.Lhs[index])
				if !ok {
					continue
				}

				if _, ok := origins[key]; !ok {
					origins[key] = struct{}{}
					changed = true
				}
			}

		case *ast.ValueSpec:
			for index, value := range statement.Values {
				if !isOriginExpression(info, value, origins) ||
					index >= len(statement.Names) {

					continue
				}

				key, ok := expressionKey(info, statement.Names[index])
				if !ok {
					continue
				}

				if _, ok := origins[key]; !ok {
					origins[key] = struct{}{}
					changed = true
				}
			}
		}

		return true
	})

	return changed
}

func isOriginExpression(info *types.Info, expr ast.Expr,
	origins map[string]struct{}) bool {

	key, ok := expressionKey(info, unwrapExpression(info, expr))
	if !ok {
		return false
	}

	_, ok = origins[key]
	return ok
}

// unwrapExpression removes expression forms which preserve the identity of a
// database executor.
func unwrapExpression(info *types.Info, expr ast.Expr) ast.Expr {
	for {
		switch value := expr.(type) {
		case *ast.ParenExpr:
			expr = value.X

		case *ast.TypeAssertExpr:
			expr = value.X

		case *ast.CallExpr:
			// A one-argument type conversion preserves the identity of
			// the converted executor.
			if len(value.Args) != 1 || !info.Types[value.Fun].IsType() {
				return expr
			}

			expr = value.Args[0]

		default:
			return expr
		}
	}
}

// expressionKey returns a stable identity for identifiers and field selector
// paths. The key uses go/types objects, so shadowed variables with the same
// spelling remain distinct.
func expressionKey(info *types.Info, expr ast.Expr) (string, bool) {
	expr = unwrapExpression(info, expr)

	switch value := expr.(type) {
	case *ast.Ident:
		object := info.ObjectOf(value)
		if object == nil {
			return "", false
		}

		return objectKey(object), true

	case *ast.SelectorExpr:
		selection := info.Selections[value]
		if selection == nil || selection.Kind() != types.FieldVal {
			return "", false
		}

		base, ok := expressionKey(info, value.X)
		if !ok {
			return "", false
		}

		return base + "/" + objectKey(selection.Obj()), true

	default:
		return "", false
	}
}

func objectKey(object types.Object) string {
	return fmt.Sprintf("%p", object)
}

func init() {
	register.Plugin(txScopeLinterName, NewTxScope)
}
