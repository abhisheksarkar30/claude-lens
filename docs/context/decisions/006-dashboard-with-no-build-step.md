[← INDEX](../INDEX.md)

# ADR 006: The dashboard is hand-written and embedded; no bundler, no CDN

**Status:** Accepted

**Context:** The dashboard needs charts. The conventional answer is a charting library — Chart.js
from a CDN being the obvious one, and the choice the source tech plan named. That brings a build
step or a runtime network dependency, and both are a problem here:

- A **runtime CDN fetch** is an offline failure mode for a loopback tool, and — more seriously — an
  exfiltration path for a tool that holds every prompt and every file the agent read.
- A **build step** means `go build` no longer produces a complete binary, and the single-static-binary
  story (which `modernc.org/sqlite`'s no-cgo choice exists to protect) stops being true.

**Decision:** The HTML, CSS, and JS are hand-written, committed as-is, and embedded with `go:embed`
([internal/web/embed.go](../../../internal/web/embed.go)). The three charts are **hand-rolled inline
SVG**. **Nothing is fetched at runtime** — not a font, not a charting library, not a CDN script. No
npm, no `package.json`, no bundler.

**Consequences:**

- `go build ./...` produces the whole tool. There is nothing to install before running it.
- The dashboard works with no network at all.
- **The no-build-step rule is enforced by tests, not by convention**, because with no bundler there
  is nothing else to catch a broken reference:
  `TestAssetsEveryLookupHasAMount`, `TestAssetsTheFourNewTabsHaveTheirMountPoints`, and
  `TestAssetsChartsAreInlineSVG` (see [../dashboard.md](../dashboard.md)). A chart that loses its
  `<title>` labels, or a tab whose mount point disappears, fails a test.
- Adding a real frontend framework is a design change, not a refactor — it would reverse this
  decision and the two above it.
- Because there is no framework, escaping is manual: every interpolated value passes through `esc()`,
  and that is load-bearing rather than decorative.
