package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func TestMain(m *testing.M) {
	// Profile defaults belong to each fixture, not the shell that runs the suite.
	if err := os.Unsetenv(profileEnv); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestParseProfiles(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want []string
	}{
		{raw: ""},
		{raw: " , \t,\n"},
		{raw: "debug", want: []string{"debug"}},
		{raw: " debug, ,metrics\t,\n tools,", want: []string{"debug", "metrics", "tools"}},
		{raw: "*", want: []string{"*"}},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			if got := parseProfiles(tt.raw); !slices.Equal(got, tt.want) {
				t.Errorf("parseProfiles(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestProfileFlagPrecedence(t *testing.T) {
	t.Setenv(profileEnv, " tools, ,metrics ")
	for _, tt := range []struct {
		name string
		args []string
		want []string
	}{
		{name: "unset uses env", want: []string{"tools", "metrics"}},
		{name: "flag replaces env", args: []string{"--profile", "debug"}, want: []string{"debug"}},
		{name: "empty flag clears env", args: []string{"-p", ""}},
		{name: "comma-only flag clears env", args: []string{"-p", ","}},
		{name: "whitespace-only flag clears env", args: []string{"-p", " \t"}},
		{name: "spaced flag values", args: []string{"-p", " debug, metrics "}, want: []string{"debug", "metrics"}},
		{name: "trailing comma", args: []string{"-p", "debug,"}, want: []string{"debug"}},
		{name: "comma separated and repeated", args: []string{"-p", "debug,tools", "--profile", "metrics"}, want: []string{"debug", "tools", "metrics"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := pflag.NewFlagSet("test", pflag.ContinueOnError)
			profiles := profileFlag(f)
			if err := f.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			if got := profiles(); !slices.Equal(got, tt.want) {
				t.Errorf("profiles = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCommandsSelectProfilesBeforeClusterAccess(t *testing.T) {
	t.Setenv(profileEnv, "")
	dir := t.TempDir()
	for name, content := range map[string]string{
		"kustomization.yaml": "resources: []\n",
		"ksync.yaml":         "allowedContexts: [dev-a, dev-b]\napps:\n  - name: web\n    path: .\n    profiles: [debug]\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"watch", "sync", "diff", "render", "images", "destroy"} {
		t.Run(name, func(t *testing.T) {
			cmd, _, err := newRootCmd().Find([]string{name})
			if err != nil {
				t.Fatal(err)
			}
			flag := cmd.Flags().Lookup("profile")
			if flag == nil || flag.Shorthand != "p" || flag.Value.Type() != "stringSlice" {
				t.Fatalf("%s must define --profile/-p as StringSlice: %v", name, flag)
			}
			err = executeKsync(t, name, "-f", filepath.Join(dir, "ksync.yaml"), "-p", "nope")
			if err == nil || !strings.Contains(err.Error(), `unknown profile "nope" (declared profiles: debug)`) {
				t.Fatalf("%s error = %v, want unknown-profile error before context selection", name, err)
			}
		})
	}
	if newRootCmd().PersistentFlags().Lookup("profile") != nil {
		t.Error("--profile must be per-command")
	}
}
