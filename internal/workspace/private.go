package workspace

import (
	"os"
	"path/filepath"
)

// Tightened is one state directory Tighten found open to other users: it
// made it private (Err nil), or its chmod failed (Err).
type Tightened struct {
	Dir string
	Err error
}

// Home is the state directory (GHOSTBRANCH_HOME) w lives in.
func (w *Workspace) Home() string { return filepath.Dir(filepath.Dir(w.Dir)) }

// StateDirs are the directories that, once private, keep everything below
// them out of other users' reach, even files an older Ghostbranch wrote
// 0644 (F-28). The home isn't one: it may be a shared or parent folder
// (even ~), so doctor only warns about it.
func (w *Workspace) StateDirs() []string {
	home := w.Home()
	return []string{filepath.Join(home, "workspaces"), w.Dir, filepath.Join(w.Dir, "pairs"),
		filepath.Join(w.Dir, "locks"), filepath.Join(home, "locks")}
}

// Tighten makes each existing state directory private (0700) when it has
// group or other bits, and reports each one it changed or couldn't. A
// directory that doesn't exist is left alone. On Windows it does nothing.
func (w *Workspace) Tighten() []Tightened {
	var out []Tightened
	for _, d := range w.StateDirs() {
		if _, err := os.Stat(d); err != nil {
			continue
		}
		if changed, err := EnsurePrivate(d); changed || err != nil {
			out = append(out, Tightened{Dir: d, Err: err})
		}
	}
	return out
}
