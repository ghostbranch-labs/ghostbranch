package progress_test

import (
	"bytes"
	"slices"
	"sync"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
)

// Compile-time checks: the three implementations satisfy the interfaces.
var (
	_ progress.Progress = progress.Nop()
	_ progress.Progress = progress.Lines(nil)
	_ progress.Progress = (*progress.Recorder)(nil)
	_ progress.Step     = progress.Nop().Step("x")
)

func TestRecorderChainsSteps(t *testing.T) {
	t.Parallel()
	var r progress.Recorder
	s1 := r.Step("Replaying x onto public main")
	r.Note(progress.Info, "public main is at abc")
	s2 := r.Step("Scanning x for leaks") // ends s1 as done
	s1.Done("too late")                  // ignored: s1 already ended
	s2.Update("Scanning x for leaks (2 of 3)")
	s2.Done("Scanned x for leaks")
	s3 := r.Step("Waiting for CI to start")
	s3.Fail()
	s3.Update("ignored")
	want := []progress.Event{
		{Kind: "step", Text: "Replaying x onto public main"},
		{Kind: "note", Text: "public main is at abc", Level: progress.Info},
		{Kind: "done", Text: "Replaying x onto public main"},
		{Kind: "step", Text: "Scanning x for leaks"},
		{Kind: "update", Text: "Scanning x for leaks (2 of 3)"},
		{Kind: "done", Text: "Scanned x for leaks"},
		{Kind: "step", Text: "Waiting for CI to start"},
		{Kind: "fail", Text: "Waiting for CI to start"},
	}
	if got := r.Events(); !slices.Equal(got, want) {
		t.Fatalf("events:\n%s", r.String())
	}
	if r.Running() {
		t.Fatal("no step should be running")
	}
	if got := r.Steps(); !slices.Equal(got, []string{"Replaying x onto public main", "Scanning x for leaks", "Waiting for CI to start"}) {
		t.Fatalf("steps = %q", got)
	}
	if got := r.Notes(); !slices.Equal(got, []string{"public main is at abc"}) {
		t.Fatalf("notes = %q", got)
	}
	if got := r.Updates(); !slices.Equal(got, []string{"Scanning x for leaks (2 of 3)"}) {
		t.Fatalf("updates = %q", got)
	}
}

func TestRecorderIsSafeAcrossGoroutines(t *testing.T) {
	t.Parallel()
	var r progress.Recorder
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := r.Step("step")
			r.Note(progress.Warn, "note")
			s.Update("update")
			if i%2 == 0 {
				s.Done("")
			}
		}()
	}
	wg.Wait()
	if n := len(r.Steps()); n != 8 {
		t.Fatalf("%d steps", n)
	}
}

// R15: plain lines print each step's start, each note, and a step's ending
// only when it carries news (its text changed since it started): "ok: …"
// or "failed: …", ASCII for logs. Updates themselves print nothing.
func TestLinesPrintsStartsNotesAndEndingsWithNews(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	p := progress.Lines(&b)
	s := p.Step("Pushing the gate candidate to the shadow repo")
	p.Note(progress.Warn, "PR #4 stays chained")
	s.Done("") // no news: nothing more
	s.Fail()   // already ended: nothing
	ci := p.Step("Waiting for CI to start")
	ci.Update("Waiting for CI (1 of 2 checks done)") // not printed
	ci.Done("CI passed")
	ci2 := p.Step("Waiting for CI to start")
	ci2.Update("CI failed: lint")
	ci2.Fail()
	same := p.Step("Replaying x onto public main")
	same.Done("Replaying x onto public main") // same text: no news
	quiet := p.Step("Scanning x")
	quiet.Fail() // no news: the error line that follows says why
	p.Step("first")
	p.Step("second") // ends "first" with no news: nothing extra
	want := "Pushing the gate candidate to the shadow repo\nPR #4 stays chained\n" +
		"Waiting for CI to start\nok: CI passed\n" +
		"Waiting for CI to start\nfailed: CI failed: lint\n" +
		"Replaying x onto public main\n" +
		"Scanning x\n" +
		"first\nsecond\n"
	if got := b.String(); got != want {
		t.Fatalf("lines =\n%q\nwant\n%q", got, want)
	}
	progress.Lines(nil).Step("a nil writer prints nothing").Done("x")
}

// F1: a step ended by the next Step ends silently, even after an Update.
func TestLinesEndsAnUpdatedStepSilentlyWhenTheNextStepStarts(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	p := progress.Lines(&b)
	s := p.Step("Scanning x")
	s.Update("Scanning x (2 of 3)")
	p.Step("Next")
	s.Done("late") // already ended: nothing
	want := "Scanning x\nNext\n"
	if got := b.String(); got != want {
		t.Fatalf("lines =\n%q\nwant\n%q", got, want)
	}
}

func TestNopStepsAreNotNil(t *testing.T) {
	t.Parallel()
	if progress.Nop().Step("x") == nil {
		t.Fatal("Nop().Step returned nil")
	}
	if progress.Lines(nil).Step("x") == nil {
		t.Fatal("Lines(nil).Step returned nil")
	}
}
