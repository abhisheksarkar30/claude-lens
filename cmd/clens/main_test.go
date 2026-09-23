package main

import (
	"bytes"
	"sort"
	"strings"
	"testing"
)

// carriedOver is the subcommands the story's outcome definition names:
// doctor and serve, br-GI-1-15's six collectors, br-GI-1-17's ten readers and
// writers, br-GI-9-04's rekey, br-GI-11-03's reprice with br-GI-11-06's
// reflag (2 + 6 + 10 + 1 + 2 = 21), and br-GI-13-07's backfill-tool-names, a
// post-convergence addition (22). It is written out rather than derived from
// the map, because a list derived from the thing under test cannot notice a
// missing entry.
var carriedOver = []string{
	// br-GI-1-01 / br-GI-1-14
	"doctor", "serve",
	// br-GI-1-15
	"ingest", "refresh", "quota", "accounts", "models", "reconcile",
	// br-GI-1-17
	"ls", "show", "tail", "stats", "sessions", "warnings", "export", "prices", "replay", "purge",
	// br-GI-9-04
	"rekey",
	// br-GI-11-03 / br-GI-11-06
	"reprice", "reflag",
	// br-GI-13-07, post-convergence addition
	"backfill-tool-names",
}

// TestEveryCarriedOverCommandIsDispatched is the bead's "all 21 subcommands
// dispatch" clause. A nil entry is a key that exists but points at nothing --
// which would panic at dispatch rather than at build, so it is checked too.
func TestEveryCarriedOverCommandIsDispatched(t *testing.T) {
	for _, name := range carriedOver {
		cmd, ok := commands[name]
		if !ok {
			t.Errorf("commands[%q] is missing; `clens %s` would report it as unimplemented", name, name)
			continue
		}
		if cmd == nil {
			t.Errorf("commands[%q] is nil; `clens %s` would panic", name, name)
		}
	}
	if len(commands) != len(carriedOver) {
		names := make([]string, 0, len(commands))
		for name := range commands {
			names = append(names, name)
		}
		sort.Strings(names)
		t.Errorf("commands has %d entries, want %d:\n%s", len(commands), len(carriedOver), strings.Join(names, "\n"))
	}
}

// TestRunReportsUsageWithNoCommand: `clens` with nothing after it is a usage
// error, not a crash.
func TestRunReportsUsageWithNoCommand(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"clens"}, &stderr); code == 0 {
		t.Fatal("run with no subcommand exited 0; want a non-zero code")
	}
	if !strings.Contains(stderr.String(), "usage: clens") {
		t.Fatalf("no usage line on stderr: %q", stderr.String())
	}
}

// TestRunExitsNonZeroOnAnUnknownCommand is the other half: a name that is not
// in the table is refused, and the message names it so a typo is visible.
func TestRunExitsNonZeroOnAnUnknownCommand(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"clens", "teleport"}, &stderr); code == 0 {
		t.Fatal("run with an unknown subcommand exited 0; want a non-zero code")
	}
	if !strings.Contains(stderr.String(), "teleport") {
		t.Fatalf("the message does not name the unknown command: %q", stderr.String())
	}
}

// TestRunExitsNonZeroWhenTheCommandFails: a subcommand's own error becomes a
// non-zero exit, with the command named so a shell script can tell which of
// several invocations failed.
func TestRunExitsNonZeroWhenTheCommandFails(t *testing.T) {
	// `replay` with no id is the cheapest failure to provoke: it rejects the
	// argv before it resolves a home directory or opens a store, so this
	// asserts the exit path without depending on the host's filesystem.
	var stderr bytes.Buffer
	if code := run([]string{"clens", "replay"}, &stderr); code == 0 {
		t.Fatal("run on a subcommand that errored exited 0; want a non-zero code")
	}
	if !strings.Contains(stderr.String(), "clens replay:") {
		t.Fatalf("the failure does not name the command: %q", stderr.String())
	}
}
