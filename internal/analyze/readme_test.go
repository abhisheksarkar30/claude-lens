package analyze

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// repoRoot is this package's distance from the checkout root. Both tests here
// read files that live there (README.md, go.mod), so a package move breaks
// them loudly rather than silently skipping.
var repoRoot = filepath.Join("..", "..")

// TestReadmeKindTableMatchesAllKinds is the clause that makes a variant
// spelling a build failure instead of a typo someone eventually finds. The
// README's kind table is the documentation a user reads; kinds.go is the
// single source of truth for the spellings; this asserts the two are the same
// set, each kind exactly once, with nothing extra.
func TestReadmeKindTableMatchesAllKinds(t *testing.T) {
	readme := readDoc(t, filepath.Join(repoRoot, "README.md"))
	rows := markdownTable(readme, "kind")
	if len(rows) == 0 {
		t.Fatal("README.md has no table whose first column header is `kind`")
	}

	declared := map[Kind]KindInfo{}
	for _, ki := range AllKinds() {
		declared[ki.Kind] = ki
	}
	if len(declared) == 0 {
		t.Fatal("AllKinds() is empty, so this test would pass vacuously")
	}

	seen := map[Kind]bool{}
	for _, row := range rows {
		// Three columns: kind | severity | emitted by. A cell containing a
		// literal `|` splits into a fourth, which is exactly the mistake this
		// catches -- markdown would render it as a mangled row, and nothing
		// else in the build would notice.
		if len(row) != 3 {
			t.Errorf("kind table row %q has %d cells, want 3 (kind | severity | emitted by)", row, len(row))
			continue
		}
		kind := Kind(strings.Trim(row[0], "`"))
		ki, ok := declared[kind]
		if !ok {
			t.Errorf("README lists kind %q, which kinds.go does not declare", kind)
			continue
		}
		if seen[kind] {
			t.Errorf("kind %q is listed twice", kind)
		}
		seen[kind] = true

		if row[1] != string(ki.Severity) {
			t.Errorf("kind %q: README says severity %q, kinds.go says %q", kind, row[1], ki.Severity)
		}

		// The four kinds emitted by another package must name that package
		// exactly as nonAnalyzeKinds does; every other kind is an analyze rule,
		// so its cell has to say so.
		if by, ok := nonAnalyzeKinds[kind]; ok {
			if row[2] != by {
				t.Errorf("kind %q: README says emitted by %q, nonAnalyzeKinds says %q", kind, row[2], by)
			}
		} else if !strings.Contains(row[2], "analyze") {
			t.Errorf("kind %q is not in nonAnalyzeKinds, so analyze emits it, but the README's emitted-by cell is %q", kind, row[2])
		}
	}

	for kind := range declared {
		if !seen[kind] {
			t.Errorf("kinds.go declares %q, which the README's kind table omits", kind)
		}
	}

	// The one spelling the plan calls out by name. Redundant with the set
	// equality above, and kept because it is the failure that actually
	// happened in review: `cache_prefix_below_minimum` misspelled as
	// `cache_prefix_below_min` still reads correctly to a human.
	if !seen["cache_prefix_below_minimum"] {
		t.Error("the README's kind table does not carry `cache_prefix_below_minimum` verbatim")
	}
}

// TestReadmeNamesEveryDirectDependency is the bead's dependency clause. The
// plan once claimed sqlite was the only non-stdlib dependency, which cannot be
// true alongside decoding brotli and zstd; the README's dependency table is
// the corrected statement, and this keeps it corrected.
//
// go.mod's own markers cannot be trusted for "direct": every require in this
// repo's go.mod is marked `// indirect`, which is stale. So the direct set is
// derived the only way that cannot go stale -- the modules the repo's own
// source actually imports, intersected with what go.mod requires.
func TestReadmeNamesEveryDirectDependency(t *testing.T) {
	readme := readDoc(t, filepath.Join(repoRoot, "README.md"))
	required := requiredModules(t, filepath.Join(repoRoot, "go.mod"))
	imported := firstPartyImports(t, repoRoot)

	direct := map[string]bool{}
	for path := range imported {
		if m := requiringModule(path, required); m != "" {
			direct[m] = true
		}
	}
	if len(direct) == 0 {
		t.Fatal("derived no direct dependencies at all, so this test would pass vacuously")
	}

	for m := range direct {
		if !strings.Contains(readme, m) {
			t.Errorf("the repo's source imports %s, but the README never names it", m)
		}
	}
}

// readDoc reads a file the tests assert on, failing with the path rather than
// an opaque stat error.
func readDoc(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// markdownTable returns the body rows of the first pipe table whose first
// column header is header, with the header and the `---` separator dropped.
// It stops at the first line that is not a table row, which is the blank line
// that ends every table here.
//
// It is a scanner for the one table shape this repo's docs use, not a
// markdown parser: cells containing escaped pipes would defeat it, and a row
// that came out with the wrong cell count is reported as such rather than
// silently reshaped.
func markdownTable(md, header string) [][]string {
	lines := strings.Split(md, "\n")
	for i, ln := range lines {
		cells, ok := tableRow(ln)
		if !ok || len(cells) == 0 || strings.Trim(cells[0], "`") != header {
			continue
		}
		var rows [][]string
		for _, body := range lines[i+2:] {
			cells, ok := tableRow(body)
			if !ok {
				break
			}
			rows = append(rows, cells)
		}
		return rows
	}
	return nil
}

// tableRow splits one `| a | b |` line into its trimmed cells.
func tableRow(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") {
		return nil, false
	}
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts, true
}

// requiredModules is every module path a `require (...)` block in go.mod
// names, whatever its `// indirect` marker says.
func requiredModules(t *testing.T, path string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	inBlock := false
	for _, ln := range strings.Split(readDoc(t, path), "\n") {
		line := strings.TrimSpace(ln)
		switch {
		case line == "require (":
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case inBlock:
			if i := strings.Index(line, "//"); i >= 0 {
				line = strings.TrimSpace(line[:i])
			}
			if f := strings.Fields(line); len(f) >= 2 {
				out[f[0]] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("parsed no requirements out of %s; the go.mod shape has changed", path)
	}
	return out
}

// firstPartyImports is every third-party import path this repo's own source
// names, under internal/ and cmd/.
func firstPartyImports(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
			if perr != nil {
				return perr
			}
			for _, imp := range f.Imports {
				path, uerr := strconv.Unquote(imp.Path.Value)
				if uerr == nil && isExternal(path) {
					out[path] = true
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", filepath.Join(root, dir), err)
		}
	}
	if len(out) == 0 {
		t.Fatal("found no third-party imports in this repo's source; the walk or the filter has broken")
	}
	return out
}

// isExternal reports whether an import path is a third-party module rather
// than the standard library, by the rule Go itself uses: a dot in the first
// path segment means a domain name, and the standard library has none.
func isExternal(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return strings.Contains(first, ".")
}

// requiringModule returns the longest go.mod requirement that is a prefix of
// imp, or "" if the import comes from somewhere go.mod does not name.
func requiringModule(imp string, required map[string]bool) string {
	best := ""
	for m := range required {
		if (imp == m || strings.HasPrefix(imp, m+"/")) && len(m) > len(best) {
			best = m
		}
	}
	return best
}
