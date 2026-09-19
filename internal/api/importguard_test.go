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

// TestAPIWebDoNotImportSecret is the credential-containment import guard
// (test 18): neither internal/api nor internal/web may import internal/secret.
// Every write route reaches a credential only through an injected
// function-value seam (SetCredentialWriter and friends), so there is no import
// edge a future handler could reach one back through.
//
// The guard reads source rather than the compile graph deliberately: an import
// that is present but only used in a branch still counts, and go/parser sees
// it whether or not the build tags would have compiled it.
func TestAPIWebDoNotImportSecret(t *testing.T) {
	for _, dir := range []string{".", filepath.Join("..", "web")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir %s: %v", dir, err)
		}
		fset := token.NewFileSet()
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
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
					t.Errorf("%s imports %q: neither internal/api nor internal/web may import internal/secret (test 18, credential containment)", path, importPath)
				}
			}
		}
	}
}
