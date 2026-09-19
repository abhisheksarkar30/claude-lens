// Command clens is the claude-lens CLI: a local observability proxy for
// Claude traffic.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/abhisheksarkar30/claude-lens/internal/cli"
)

// commands dispatches subcommand names to their internal/cli implementation.
// br-GI-1-15 appends its six entries below; br-GI-1-17 appends its twelve
// carried-over names to the same map -- the two beads own disjoint keys.
var commands = map[string]func([]string) error{
	"ingest":    cli.Ingest,
	"refresh":   cli.Refresh,
	"quota":     cli.Quota,
	"accounts":  cli.Accounts,
	"models":    cli.Models,
	"reconcile": cli.Reconcile,

	// br-GI-1-17's twelve. doctor is br-GI-1-01's implementation, extended
	// by br-GI-1-14; the rest are this bead's.
	"doctor":   cli.Doctor,
	"serve":    cli.Serve,
	"ls":       cli.Ls,
	"show":     cli.Show,
	"tail":     cli.Tail,
	"stats":    cli.Stats,
	"sessions": cli.Sessions,
	"warnings": cli.Warnings,
	"export":   cli.Export,
	"prices":   cli.Prices,
	"replay":   cli.Replay,
	"purge":    cli.Purge,
}

func main() {
	os.Exit(run(os.Args, os.Stderr))
}

// run is main's body with the exit code returned rather than exited with, and
// the diagnostics written to a caller-supplied writer. Both are what make the
// dispatch table and its failure modes reachable from a test -- an os.Exit
// mid-test takes the test binary with it.
func run(args []string, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: clens <command> [flags]")
		return 1
	}

	name := args[1]
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(stderr, "clens %s: not implemented yet\n", name)
		return 1
	}

	if err := cmd(args[2:]); err != nil {
		fmt.Fprintf(stderr, "clens %s: %v\n", name, err)
		return 1
	}
	return 0
}
