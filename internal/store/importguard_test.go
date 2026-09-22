package store

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenImports are the packages internal/store must never depend on.
//
// internal/pricing is the load-bearing one: the store is the write authority
// and the pricing engine stays behind the PriceComputer seam (store.go), the
// same boundary internal/proxy holds against sink/config. A direct import
// would compile — which is exactly why the constraint needs a test that reads
// the source rather than a comment that asks nicely.
var forbiddenImports = []string{
	"github.com/abhisheksarkar30/claude-lens/internal/pricing",
}

// TestStoreDoesNotImportPricing mirrors internal/proxy's
// TestProxyImportsAreNarrow one-for-one. The pattern is deliberately
// duplicated per package rather than shared: internal/api's own guard records
// why, and a shared helper would live in whichever package owned it, giving
// that package an import the others must not have.
func TestStoreDoesNotImportPricing(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, forbidden := range forbiddenImports {
				if path == forbidden {
					t.Errorf("%s imports %q, which internal/store must never depend on: price through the PriceComputer seam instead", file, path)
				}
			}
		}
	}
}
