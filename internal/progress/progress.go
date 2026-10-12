// Package progress is how the engine says what a slow command is doing:
// named steps, one running at a time, and one-off notes. The CLI draws them
// (an animated checklist on a terminal, plain lines elsewhere); the engine
// never formats for a terminal. A nil Progress in the engine, or Nop, prints
// nothing.
//
// Every implementation follows the same rules: starting a step finishes the
// running one as done (reaching the next step means the last one worked);
// Update, Done and Fail on an ended step do nothing; every method is safe
// from any goroutine.
package progress

import (
	"io"
	"sync"
)

// NoteLevel is how much a note matters.
type NoteLevel int

const (
	// Info is a remark: drawn as a dim "·" line.
	Info NoteLevel = iota
	// Warn is something a person should notice: a yellow "!" line.
	Warn
)

// Progress receives a command's steps and notes.
type Progress interface {
	// Step starts a step; it becomes the running (spinning) line.
	Step(text string) Step
	// Note prints a one-off line that stays, above the running step.
	Note(level NoteLevel, text string)
}

// Step is one running step.
type Step interface {
	// Update replaces the running step's text.
	Update(text string)
	// Done ends the step with a ✓. text replaces the step's text (a past
	// tense, say); "" keeps it.
	Done(text string)
	// Fail ends the step with a ✗ and its current text.
	Fail()
}

// Nop prints nothing.
func Nop() Progress { return nop{} }

type nop struct{}

func (nop) Step(string) Step       { return nopStep{} }
func (nop) Note(NoteLevel, string) {}

type nopStep struct{}

func (nopStep) Update(string) {}
func (nopStep) Done(string)   {}
func (nopStep) Fail()         {}

// Lines writes progress as plain lines, for pipes, files, CI, --json and
// TERM=dumb: each step's start and each note as one line, as Ghostbranch did
// before 6.4a. A step explicitly ended by Done or Fail prints one more line
// only when it carries news, that is when its text changed since it started
// (an Update, or a Done with a new text): "ok: <text>" or "failed: <text>".
// A step ended by the next step prints nothing. The prefixes are ASCII so
// logs stay greppable (owner, 2026-10-11; R15). Updates themselves print
// nothing. A nil w prints nothing.
func Lines(w io.Writer) Progress {
	if w == nil {
		return nop{}
	}
	return &lines{w: w}
}

type lines struct {
	mu   sync.Mutex
	w    io.Writer
	open *lineStep
}

type lineStep struct {
	l           *lines
	start, text string
	ended       bool
}

func (l *lines) Step(text string) Step {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open != nil {
		l.end(l.open, "", "") // a new step ends the last one, silently
	}
	s := &lineStep{l: l, start: text, text: text}
	l.open = s
	l.print(text)
	return s
}

func (l *lines) Note(_ NoteLevel, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.print(text)
}

// print writes one line; l.mu is held.
func (l *lines) print(s string) { _, _ = io.WriteString(l.w, s+"\n") }

// end finishes s; l.mu is held. With a prefix it prints the ending when it is
// news; with an empty prefix it ends silently.
func (l *lines) end(s *lineStep, prefix, text string) {
	if s.ended {
		return
	}
	s.ended = true
	if text != "" {
		s.text = text
	}
	if l.open == s {
		l.open = nil
	}
	if prefix != "" && s.text != s.start {
		l.print(prefix + s.text)
	}
}

func (s *lineStep) Update(text string) {
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	if !s.ended {
		s.text = text
	}
}

func (s *lineStep) Done(text string) {
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	s.l.end(s, "ok: ", text)
}

func (s *lineStep) Fail() {
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	s.l.end(s, "failed: ", "")
}
