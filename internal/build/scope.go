package build

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"

	"github.com/motoki317/ksync/internal/config"
)

// Scope is the watch surface of one build entry: the roots to observe and
// which paths beneath them cannot affect the image.
type Scope struct {
	// Roots are the paths a watcher must observe for this build.
	Roots []string

	context string
	matcher *patternmatcher.PatternMatcher // nil: nothing is ignored
	// exempt paths are image-relevant even when the ignore patterns match
	// them (docker reads the Dockerfile and the ignore file regardless of
	// the context content).
	exempt map[string]bool
}

// WatchScope derives the Scope of b. A file `.dockerignore` excludes is not
// part of the build context, so it cannot change the image — it is neither
// watched nor allowed to dirty the build. That rule is what prevents
// self-triggered rebuild loops (command builds writing artifacts into the
// context) and per-directory watch descriptors on huge trees.
// `<dockerfile>.dockerignore` takes precedence over `<context>/.dockerignore`
// (BuildKit semantics).
func WatchScope(b config.Build) (*Scope, error) {
	s := &Scope{context: b.Context, exempt: map[string]bool{}}
	if len(b.Watch) > 0 {
		s.Roots = append(s.Roots, b.Watch...)
	} else {
		s.Roots = append(s.Roots, b.Context)
	}
	if b.Dockerfile != "" {
		s.Roots = append(s.Roots, b.Dockerfile)
		s.exempt[b.Dockerfile] = true
	}

	ignoreFile := filepath.Join(b.Context, ".dockerignore")
	if b.Dockerfile != "" {
		if specific := b.Dockerfile + ".dockerignore"; fileExists(specific) {
			ignoreFile = specific
		}
	}
	// On ignore-file errors the scope is still returned usable, just without
	// ignore rules — over-watching is the safe direction (extra rebuilds,
	// never missed ones); the caller logs the error.
	f, err := os.Open(ignoreFile)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	defer func() { _ = f.Close() }()
	patterns, err := ignorefile.ReadAll(f)
	if err != nil {
		return s, err
	}
	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		return s, err
	}
	s.matcher = matcher
	s.Roots = append(s.Roots, ignoreFile)
	s.exempt[ignoreFile] = true
	return s, nil
}

// Ignored reports whether a change at path cannot affect the image.
func (s *Scope) Ignored(path string) bool {
	if s.matcher == nil {
		return false
	}
	path = filepath.Clean(path)
	if s.exempt[path] {
		return false
	}
	rel, ok := s.rel(path)
	if !ok {
		return false
	}
	matched, err := s.matcher.MatchesOrParentMatches(rel)
	return err == nil && matched
}

// SkipDir reports whether a watcher may prune the whole directory: it is
// excluded and no negation pattern could re-include anything beneath it —
// docker's own context-walk heuristic.
func (s *Scope) SkipDir(dir string) bool {
	if s.matcher == nil {
		return false
	}
	rel, ok := s.rel(filepath.Clean(dir))
	if !ok || rel == "." {
		return false
	}
	matched, err := s.matcher.MatchesOrParentMatches(rel)
	if err != nil || !matched {
		return false
	}
	if !s.matcher.Exclusions() {
		return true
	}
	prefix := rel + "/"
	for _, p := range s.matcher.Patterns() {
		if p.Exclusion() && strings.HasPrefix(filepath.ToSlash(p.String())+"/", prefix) {
			return false
		}
	}
	return true
}

// rel converts path to the slash-separated context-relative form the ignore
// patterns are written against; ok is false outside the context.
func (s *Scope) rel(path string) (string, bool) {
	rel, err := filepath.Rel(s.context, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
