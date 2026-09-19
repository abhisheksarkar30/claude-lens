// Command clens is the claude-lens CLI: a local observability proxy for
// Claude traffic.
package main

import (
	"fmt"
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
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: clens <command> [flags]")
		os.Exit(1)
	}

	name := os.Args[1]
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "clens %s: not implemented yet\n", name)
		os.Exit(1)
	}

	if err := cmd(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "clens %s: %v\n", name, err)
		os.Exit(1)
	}
}
