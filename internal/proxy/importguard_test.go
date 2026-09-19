package proxy

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenImports are the packages internal/proxy must never depend on —
// CLAUDE.md's "internal/proxy depends only on sink and config" invariant.
// Importing any of these would put a cold-path concern (decoding, storage,
// pricing, credentials) on the hot path's goroutine.
var forbiddenImports = []string{
	"internal/analyze",
	"internal/store",
	"internal/pricing",
	"internal/consumer",
	"internal/secret",
	"internal/jsonlogs",
	"internal/snapshot",
	"internal/adminrep",
}

func TestProxyImportsAreNarrow(t *testing.T) {
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
				if strings.Contains(path, forbidden) {
					t.Errorf("%s imports %q, which internal/proxy must never depend on", file, path)
				}
			}
		}
	}
}
