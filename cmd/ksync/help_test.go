package main

import (
	"regexp"
	"testing"
)

// exampleFlag matches a --long flag token inside an Example block.
var exampleFlag = regexp.MustCompile(`--[a-z][a-z-]*`)

// helpRef matches a "ksync help <topic>" cross-reference in the help prose.
var helpRef = regexp.MustCompile(`ksync help ([a-z]+)`)

// TestHelpCrossReferencesResolve guards the "help is the guide" invariant against
// dead-end cross-references: every "ksync help <topic>" the help prose points at must
// be a real concept topic, and the five documented topics must all be registered.
func TestHelpCrossReferencesResolve(t *testing.T) {
	root := newRootCmd()
	topics := map[string]bool{}
	corpus := root.Long
	for _, cmd := range root.Commands() {
		if cmd.GroupID == groupTopics {
			topics[cmd.Name()] = true
		}
		corpus += "\n" + cmd.Long + "\n" + cmd.Example
	}
	if len(topics) != 5 {
		t.Errorf("expected 5 concept topics, got %d", len(topics))
	}
	for _, m := range helpRef.FindAllStringSubmatch(corpus, -1) {
		if !topics[m[1]] {
			t.Errorf("help prose references 'ksync help %s', which is not a registered topic", m[1])
		}
	}
}

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
