package watch

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// Root is one watched root. Skip prunes directories beneath it whose content
// cannot matter (dockerignored build-context subtrees) — pruning matters
// beyond noise: fsnotify on macOS holds one kqueue descriptor per watched
// directory, so walking a large excluded tree exhausts the fd limit. nil
// watches everything under Path.
type Root struct {
	Path string
	Skip func(dir string) bool
}

// Watcher reports file changes under a set of roots, recursively. fsnotify
// only watches single directories, so the Watcher walks each root at
// SetRoots time and registers directories created later as they appear.
type Watcher struct {
	fsw *fsnotify.Watcher

	mu    sync.Mutex
	roots []Root

	// Events carries changed file paths, already filtered of VCS/editor
	// noise. Closed when the watcher closes.
	Events chan string
	// Errors carries watch-infrastructure errors (the loop logs and
	// continues; a broken watch is not fatal to running syncs).
	Errors chan error
}

func NewWatcher(roots []Root) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{fsw: fsw, Events: make(chan string, 256), Errors: make(chan error, 1)}
	if err := w.SetRoots(roots); err != nil {
		_ = fsw.Close()
		return nil, err
	}
	go w.run()
	return w, nil
}

// SetRoots replaces the root set and starts watching roots not yet observed.
// Roots that disappeared from the set stay registered with fsnotify — the
// mapping decides relevance of events, not the watcher — and roots that do
// not exist yet are skipped; a later SetRoots picks them up once created.
func (w *Watcher) SetRoots(roots []Root) error {
	w.mu.Lock()
	w.roots = roots
	w.mu.Unlock()
	var firstErr error
	for _, r := range roots {
		if err := w.add(r.Path); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// add starts watching a root. Directories are walked recursively; for a file
// root its parent directory is watched (events for siblings are filtered out
// downstream by the Mapping).
func (w *Watcher) add(root string) error {
	st, err := os.Stat(root)
	if err != nil {
		return nil
	}
	if !st.IsDir() {
		return w.fsw.Add(filepath.Dir(root))
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" {
			return fs.SkipDir
		}
		if p != root && w.skippable(p) {
			return fs.SkipDir
		}
		return w.fsw.Add(p)
	})
}

// skippable reports whether every root covering dir prunes it. One covering
// root that wants the directory watched keeps it watched — roots overlap
// (a manifest dir inside a build context, two builds sharing a context), and
// pruning is only safe when no observer cares.
func (w *Watcher) skippable(dir string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	covered := false
	for _, r := range w.roots {
		if dir != r.Path && !isBelow(r.Path, dir) {
			continue
		}
		if r.Skip == nil || !r.Skip(dir) {
			return false
		}
		covered = true
	}
	return covered
}

func (w *Watcher) Close() error {
	return w.fsw.Close()
}

func (w *Watcher) run() {
	for {
		select {
		case ev, ok := <-w.fsw.Events:
			if !ok {
				close(w.Events)
				return
			}
			// Chmod-only events carry no content change (macOS surfaces
			// plain mtime updates this way too, but those pair with Write).
			if ev.Op == fsnotify.Chmod || Ignored(ev.Name) {
				continue
			}
			if ev.Op&fsnotify.Create != 0 {
				if st, err := os.Stat(ev.Name); err == nil && st.IsDir() && !w.skippable(ev.Name) {
					_ = w.add(ev.Name) // best-effort; a failed add surfaces as missed events, not a crash
				}
			}
			w.Events <- ev.Name
		case err, ok := <-w.fsw.Errors:
			if !ok {
				close(w.Errors)
				return
			}
			w.Errors <- err
		}
	}
}

// Ignored reports paths whose changes can never affect rendered output: VCS
// internals and editor temp/backup/lock files.
func Ignored(path string) bool {
	for _, seg := range strings.Split(path, string(filepath.Separator)) {
		if seg == ".git" {
			return true
		}
	}
	base := filepath.Base(path)
	switch {
	case strings.HasSuffix(base, "~"), // backup files
		strings.HasSuffix(base, ".swp"), strings.HasSuffix(base, ".swx"), // vim swap
		strings.HasPrefix(base, ".#"), // emacs locks
		base == "4913":                // vim's write-permission probe
		return true
	}
	return false
}
