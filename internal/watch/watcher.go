package watch

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

// Watcher reports file changes under a set of roots, recursively. fsnotify
// only watches single directories, so the Watcher walks each root at Add time
// and registers directories created later as they appear.
type Watcher struct {
	fsw *fsnotify.Watcher
	// Events carries changed file paths, already filtered of VCS/editor
	// noise. Closed when the watcher closes.
	Events chan string
	// Errors carries watch-infrastructure errors (the loop logs and
	// continues; a broken watch is not fatal to running syncs).
	Errors chan error
}

func NewWatcher(roots []string) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{fsw: fsw, Events: make(chan string, 256), Errors: make(chan error, 1)}
	for _, root := range roots {
		if err := w.Add(root); err != nil {
			_ = fsw.Close()
			return nil, err
		}
	}
	go w.run()
	return w, nil
}

// Add starts watching a root. Directories are walked recursively; for a file
// root its parent directory is watched (events for siblings are filtered out
// downstream by the Mapping). A root that does not exist yet is skipped —
// re-Adding after dependency roots are re-derived picks it up once created.
func (w *Watcher) Add(root string) error {
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
		return w.fsw.Add(p)
	})
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
				if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
					_ = w.Add(ev.Name) // best-effort; a failed add surfaces as missed events, not a crash
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
