package api

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestAPIDoesNotImportSecret is the credential-containment import guard
// (test 18): internal/api must never import internal/secret, since every
// write route reaches a credential only through an injected function-value
// seam (SetPricing here; SetCredentialWriter etc. land with the write
// routes in a later slice). internal/web doesn't exist yet in this slice --
// extend this guard to cover it once it does.
func TestAPIDoesNotImportSecret(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := filepath.Join(".", entry.Name())
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if strings.Contains(importPath, "/internal/secret") {
				t.Errorf("%s imports %q: internal/api must never import internal/secret (test 18, credential containment)", entry.Name(), importPath)
			}
		}
	}
}
