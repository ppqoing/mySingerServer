//go:build cgo && windows && legacy_mediacore

package mediacore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPhase2GoCallsOnlyCountedCWrappers(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate phase2_ast_test.go")
	}
	sourcePath := filepath.Join(filepath.Dir(testFile), "phase2.go")
	files := token.NewFileSet()
	source, err := parser.ParseFile(files, sourcePath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	forbidden := map[string]struct{}{
		"mc_decode_gray":      {},
		"mc_pdq256_from_gray": {},
		"mc_phase2_image":     {},
		"mc_phash_parts":      {},
		"mc_sobel_hist":       {},
	}
	ast.Inspect(source, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		packageName, ok := selector.X.(*ast.Ident)
		if !ok || packageName.Name != "C" {
			return true
		}
		if _, prohibited := forbidden[selector.Sel.Name]; prohibited {
			t.Errorf(
				"%s uses forbidden raw C selector C.%s",
				files.Position(selector.Pos()),
				selector.Sel.Name,
			)
		}
		return true
	})
}
