// Command clens is the claude-lens CLI: a local observability proxy for
// Claude traffic.
package main

import (
	"fmt"
	"os"
)

// commands dispatches subcommand names to their internal/cli implementation.
// Empty in this bead; br-GI-1-15 and br-GI-1-17 append their entries here.
var commands = map[string]func([]string) error{}

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
