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

// forbiddenImports is the containment the api/web package docs promise:
// neither package may import secret (a credential), config (an account's
// plan and billing mode), or ingest (a collector's health) -- each write
// route reaches those through an injected function-value seam
// (SetCredentialWriter and friends) instead, so there is no import edge a
// future handler could reach one back through.
//
// testFiles says whether the ban covers _test.go files too. secret's does: a
// test has no business opening the credential store either. config's and
// ingest's do not -- internal/api/replay_test.go takes config.Account values
// to build a fixture, which is a data dependency, not a write path.
//
// The config/ingest half overlaps TestWriteSeamsDoNotImportConfigOrIngest in
// internal/cli/serve_test.go, which asserts the same two edges for the same
// two packages. The duplication is deliberate and cheap: bead 17's acceptance
// criteria name the cli-side guard, and the containment claim in the api/web
// package docs is what this one answers. A future third package would have to
// be added to both.
var forbiddenImports = []struct {
	path      string
	testFiles bool
}{
	{"/internal/secret", true},
	{"/internal/config", false},
	{"/internal/ingest", false},
}

// TestAPIWebImportGuard is the containment import guard (test 18). Every
// write route reaches a credential only through an injected function-value
// seam (SetCredentialWriter and friends), so there is no import edge a
// future handler could reach one back through.
//
// The guard reads source rather than the compile graph deliberately: an import
// that is present but only used in a branch still counts, and go/parser sees
// it whether or not the build tags would have compiled it.
func TestAPIWebImportGuard(t *testing.T) {
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
			isTest := strings.HasSuffix(entry.Name(), "_test.go")
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
				for _, bad := range forbiddenImports {
					if isTest && !bad.testFiles {
						continue
					}
					if strings.Contains(importPath, bad.path) {
						t.Errorf("%s imports %q: neither internal/api nor internal/web may import %s (test 18, containment)", path, importPath, bad.path)
					}
				}
			}
		}
	}
}
