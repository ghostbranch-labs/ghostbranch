package progress

import (
	"fmt"
	"strings"
	"sync"
)

// Event is one thing a Recorder saw. Kind is "step", "update", "done",
// "fail" or "note"; Level is set for notes only.
type Event struct {
	Kind  string
	Text  string
	Level NoteLevel
}

// Recorder keeps every step, update and note in order, for tests. The zero
// value is ready to use.
type Recorder struct {
	mu     sync.Mutex
	events []Event
	open   *recStep
}

type recStep struct {
	r     *Recorder
	text  string
	ended bool
}

func (r *Recorder) Step(text string) Step {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endOpen()
	s := &recStep{r: r, text: text}
	r.open = s
	r.events = append(r.events, Event{Kind: "step", Text: text})
	return s
}

// endOpen finishes the running step as done; r.mu is held.
func (r *Recorder) endOpen() {
	if r.open != nil && !r.open.ended {
		r.open.ended = true
		r.events = append(r.events, Event{Kind: "done", Text: r.open.text})
	}
	r.open = nil
}

func (r *Recorder) Note(level NoteLevel, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, Event{Kind: "note", Text: text, Level: level})
}

func (s *recStep) Update(text string) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if s.ended {
		return
	}
	s.text = text
	s.r.events = append(s.r.events, Event{Kind: "update", Text: text})
}

func (s *recStep) Done(text string) { s.end("done", text) }
func (s *recStep) Fail()            { s.end("fail", "") }

func (s *recStep) end(kind, text string) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if s.ended {
		return
	}
	s.ended = true
	if text == "" {
		text = s.text
	}
	s.r.events = append(s.r.events, Event{Kind: kind, Text: text})
	if s.r.open == s {
		s.r.open = nil
	}
}

// Events returns a copy of everything seen, in order.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

func (r *Recorder) texts(kind string) []string {
	var out []string
	for _, e := range r.Events() {
		if e.Kind == kind {
			out = append(out, e.Text)
		}
	}
	return out
}

// Steps is the text each step started with, in order.
func (r *Recorder) Steps() []string { return r.texts("step") }

// Updates is every update's text, in order.
func (r *Recorder) Updates() []string { return r.texts("update") }

// Notes is every note's text, in order.
func (r *Recorder) Notes() []string { return r.texts("note") }

// Running reports whether a step was started and never ended.
func (r *Recorder) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.open != nil && !r.open.ended
}

// String is one "kind: text" line per event, for test failure messages.
func (r *Recorder) String() string {
	var b strings.Builder
	for _, e := range r.Events() {
		fmt.Fprintf(&b, "%s: %s\n", e.Kind, e.Text)
	}
	return b.String()
}
