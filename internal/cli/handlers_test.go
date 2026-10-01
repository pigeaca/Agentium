package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// maxHandlerLines is the longest a command handler may be: handlers parse flags, call one service in the package that
// owns the logic, and print. Longer ones hold business logic that belongs in a service (the refactor round, step 2).
const maxHandlerLines = 80

// tooLongHandlers are handlers over the limit that wait for another change to land first. Do not add to it: shorten
// the handler instead.
var tooLongHandlers = map[string]bool{
	// task.go and task_mine.go are being changed by the task-mining PR (#62); taskValidate moves to a service when it
	// merges.
	"taskValidate": true,
}

// A command handler is a function of the shape func(ctx context.Context, env Env, args []string) int.
func TestCommandHandlersStayShort(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found, listed := 0, map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range src.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil || !isHandler(fn.Type) {
				continue
			}
			found++
			lines := fset.Position(fn.End()).Line - fset.Position(fn.Pos()).Line + 1
			switch {
			case tooLongHandlers[fn.Name.Name]:
				listed[fn.Name.Name] = true
				if lines <= maxHandlerLines {
					t.Errorf("%s is %d lines: remove it from tooLongHandlers", fn.Name.Name, lines)
				}
			case lines > maxHandlerLines:
				t.Errorf("%s (%s) is %d lines, over %d: move its logic into the package that owns it", fn.Name.Name, file, lines, maxHandlerLines)
			}
		}
	}
	if found < 10 {
		t.Errorf("found %d handlers: has the handler signature changed? Update isHandler", found)
	}
	for name := range tooLongHandlers {
		if !listed[name] {
			t.Errorf("tooLongHandlers lists %s, which is not a handler", name)
		}
	}
}

// isHandler reports whether ft is (context.Context, Env, []string) int.
func isHandler(ft *ast.FuncType) bool {
	if ft.Params == nil || ft.Results == nil || len(ft.Results.List) != 1 || ft.Params.NumFields() != 3 {
		return false
	}
	var types []string
	for _, f := range ft.Params.List {
		for range max(len(f.Names), 1) {
			types = append(types, exprString(f.Type))
		}
	}
	return strings.Join(types, ",") == "context.Context,Env,[]string" && exprString(ft.Results.List[0].Type) == "int"
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.ArrayType:
		return "[]" + exprString(x.Elt)
	}
	return "?"
}
