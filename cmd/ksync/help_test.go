package main

import (
	"regexp"
	"testing"
)

// exampleFlag matches a --long flag token inside an Example block.
var exampleFlag = regexp.MustCompile(`--[a-z][a-z-]*`)

// TestCommandExamplesReferenceRealFlags guards the "help is the guide" invariant
// against flag-name drift: every --flag shown in a command's Example must be a real
// flag on that command, so a rename in cli.go can never leave a stale example. It
// deliberately does NOT check example values — a well-formed but wrong IMAGE=REF
// passes here; running the examples against a cluster is qa-review's job.
func TestCommandExamplesReferenceRealFlags(t *testing.T) {
	for _, cmd := range newRootCmd().Commands() {
		if cmd.Example == "" {
			continue
		}
		for _, tok := range exampleFlag.FindAllString(cmd.Example, -1) {
			name := tok[len("--"):]
			if cmd.Flags().Lookup(name) == nil {
				t.Errorf("%s example references --%s, which is not a flag on that command", cmd.Name(), name)
			}
		}
	}
}
