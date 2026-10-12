package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli/style"
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func testChecklist(width *int) (*checklist, *bytes.Buffer, *testutil.Clock) {
	var b bytes.Buffer
	clock := testutil.NewClock(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	c := newChecklist(&b, style.New(false, false), clock.Now, func() int { return *width })
	return c, &b, clock
}

func TestElapsed(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		999 * time.Millisecond: "", time.Second: "1s", 59 * time.Second: "59s",
		72 * time.Second: "1m12s", 3600 * time.Second: "1h00m", 3725 * time.Second: "1h02m",
	} {
		if got := elapsed(d); got != want {
			t.Errorf("elapsed(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestFit(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		mark, text, timer string
		cols              int
		want              string
	}{
		{"✓", "Scanned 12 commits for leaks", "2s", 80, "✓ Scanned 12 commits for leaks" + strings.Repeat(" ", 26) + "  2s"},
		{"⠋", "Waiting for CI", "", 80, "⠋ Waiting for CI"},
		{"⠋", "A very long step text", "", 12, "⠋ A very l…"},
		{"⠋", "Waiting", "1m12s", 12, "⠋ Waiting"}, // too narrow for the timer: it goes
		{"✓", "tab\tand\nnewline", "", 80, "✓ tab and newline"},
	} {
		if got := fit(c.mark, c.text, c.timer, c.cols); got != c.want {
			t.Errorf("fit(%q, %q, %q, %d) = %q, want %q", c.mark, c.text, c.timer, c.cols, got, c.want)
		}
	}
}

// Review Focus 5: no width, however small, and no text makes a line wrap or
// carry a control character.
func TestFitNeverExceedsTheWidth(t *testing.T) {
	t.Parallel()
	text := "Replaying ünïcødé-feature 機能 onto public main\x1b[31m and more words"
	for cols := 0; cols <= 40; cols++ {
		for _, timer := range []string{"", "9s", "1h02m"} {
			got := fit("⠹", text, timer, cols)
			limit := cols - 1
			if cols <= 0 {
				limit = 79
			}
			if n := utf8.RuneCountInString(got); n > limit && limit >= 1 {
				t.Fatalf("cols %d timer %q: %d runes: %q", cols, timer, n, got)
			}
			if strings.ContainsAny(got, "\x1b\n\r\t") {
				t.Fatalf("control character in %q", got)
			}
		}
	}
}

func TestChecklistDrawsTicksAndKeepsDoneLines(t *testing.T) {
	t.Parallel()
	w := 80
	c, b, clock := testChecklist(&w)
	s := c.Step("Scanning")
	c.tick()
	clock.Sleep(2 * time.Second)
	s.Done("Scanned 12 commits for leaks")
	want := hideCursor + clearLine + "⠋ Scanning" +
		clearLine + "⠙ Scanning" +
		clearLine + fit("✓", "Scanned 12 commits for leaks", "2s", 80) + "\n" + showCursor
	if b.String() != want {
		t.Fatalf("frames:\n%q\nwant\n%q", b.String(), want)
	}
}

func TestChecklistTimerShowsFromOneSecond(t *testing.T) {
	t.Parallel()
	w := 80
	c, b, clock := testChecklist(&w)
	c.Step("Waiting for CI")
	clock.Sleep(900 * time.Millisecond)
	c.tick()
	if strings.Contains(b.String(), "0s") {
		t.Fatalf("a timer under 1s: %q", b.String())
	}
	clock.Sleep(200 * time.Millisecond)
	c.tick()
	if !strings.HasSuffix(b.String(), "  1s") {
		t.Fatalf("no timer at 1.1s: %q", b.String())
	}
}

func TestChecklistFailLineAndNewStepEndsTheLast(t *testing.T) {
	t.Parallel()
	w := 80
	c, b, _ := testChecklist(&w)
	c.Step("Replaying x")
	s := c.Step("Pushing") // ends "Replaying x" as ✓
	s.Fail()
	out := b.String()
	if !strings.Contains(out, "✓ Replaying x\n") || !strings.Contains(out, "✗ Pushing\n") || !strings.HasSuffix(out, showCursor) {
		t.Fatalf("%q", out)
	}
}

func TestChecklistCutsToTheWidthAndFollowsAChange(t *testing.T) {
	t.Parallel()
	w := 20
	c, b, _ := testChecklist(&w)
	c.Step("Waiting for CI (2 of 6 checks done)")
	if !strings.HasSuffix(b.String(), "⠋ Waiting for CI (…") {
		t.Fatalf("not cut to 19: %q", b.String())
	}
	w = 50
	c.tick()
	if !strings.HasSuffix(b.String(), "⠙ Waiting for CI (2 of 6 checks done)") {
		t.Fatalf("did not follow the wider window: %q", b.String())
	}
}

func TestChecklistNotesGoAboveTheSpinner(t *testing.T) {
	t.Parallel()
	w := 80
	c, b, _ := testChecklist(&w)
	c.Step("Publishing")
	b.Reset()
	c.Note(progress.Warn, "PR #4 stays chained")
	c.Note(progress.Info, "commit abc → acme/widget:feat-x")
	want := clearLine + "! PR #4 stays chained\n" + clearLine + "⠋ Publishing" +
		clearLine + "· commit abc → acme/widget:feat-x\n" + clearLine + "⠋ Publishing"
	if b.String() != want {
		t.Fatalf("%q\nwant\n%q", b.String(), want)
	}
}

func TestChecklistPausesForAPrompt(t *testing.T) {
	t.Parallel()
	w := 80
	c, b, _ := testChecklist(&w)
	c.Step("Publishing")
	b.Reset()
	c.Pause()
	c.tick()
	if b.String() != clearLine+showCursor {
		t.Fatalf("pause: %q", b.String())
	}
	b.Reset()
	c.Resume()
	if b.String() != hideCursor+clearLine+"⠙ Publishing" {
		t.Fatalf("resume: %q", b.String())
	}
}

// Review Focus 4: after Finish nothing spins, the cursor is shown, and what
// is written next starts on a clean line.
func TestChecklistFinishLeavesACleanLine(t *testing.T) {
	t.Parallel()
	w := 80
	c, b, _ := testChecklist(&w)
	c.Step("Waiting for CI")
	c.Finish(true)
	c.Finish(true) // idempotent
	b.WriteString("error: interrupted\n")
	out := b.String()
	if !strings.HasSuffix(out, "✗ Waiting for CI\n"+showCursor+"error: interrupted\n") {
		t.Fatalf("%q", out)
	}
	if strings.LastIndex(out, hideCursor) > strings.LastIndex(out, showCursor) {
		t.Fatal("the cursor stays hidden")
	}
}

// A step left open on success is closed without a mark; after Finish, steps
// print as plain lines.
func TestChecklistClosesAnOpenStepWithoutAMark(t *testing.T) {
	t.Parallel()
	w := 80
	c, b, _ := testChecklist(&w)
	c.Step("Reading acme/widget")
	c.Finish(false)
	c.Step("late step").Done("")
	c.Note(progress.Info, "late note")
	if !strings.HasSuffix(b.String(), "  Reading acme/widget\n"+showCursor+"late step\nlate note\n") {
		t.Fatalf("%q", b.String())
	}
}

// R16: spinner only, for read-only commands. Done steps (explicit, implicit
// or closed by Finish) leave nothing; a ✗ and notes stay; the cursor ends
// shown and no stale step text is left on the line.
func TestQuietChecklistKeepsOnlyFailuresAndNotes(t *testing.T) {
	t.Parallel()
	w := 80
	c, b, _ := testChecklist(&w)
	c.setQuiet(true)
	c.Step("Reading acme/widget").Done("")
	if !strings.HasSuffix(b.String(), clearLine+showCursor) {
		t.Fatalf("a finished quiet step must clear its line: %q", b.String())
	}
	c.Step("Surveying features")
	c.Note(progress.Info, "a note")
	c.Step("Scanning").Fail() // ends "Surveying features" silently
	c.Step("Last")
	c.Finish(false)
	out := b.String()
	if strings.Contains(out, "✓") || strings.Contains(out, "  Last\n") || strings.Contains(out, "Surveying features\n") ||
		!strings.Contains(out, "· a note\n") || !strings.Contains(out, "✗ Scanning\n") || !strings.HasSuffix(out, showCursor) {
		t.Fatalf("%q", out)
	}
}

func TestChecklistColoursTheMarks(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	clock := testutil.NewClock(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	c := newChecklist(&b, style.New(true, false), clock.Now, func() int { return 80 })
	c.Step("a").Done("")
	c.Step("b").Fail()
	c.Note(progress.Warn, "w")
	for _, want := range []string{"\x1b[92m✓\x1b[0m a", "\x1b[91m✗\x1b[0m b", "\x1b[93m!\x1b[0m w"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("lacks %q in %q", want, b.String())
		}
	}
}

func TestChecklistIsSafeAcrossGoroutines(t *testing.T) {
	t.Parallel()
	w := 80
	c, b, _ := testChecklist(&w)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := c.Step("s")
			c.Note(progress.Info, "n")
			c.tick()
			c.Pause()
			c.Resume()
			s.Update("u")
			s.Done("")
		}()
	}
	wg.Wait()
	c.Finish(false)
	// Every step ends exactly once as a ✓ line (by Done or by the next Step),
	// every note stays, and the cursor ends shown.
	out := b.String()
	if n := strings.Count(out, "✓ "); n != 8 {
		t.Errorf("%d ✓ lines, want 8: %q", n, out)
	}
	if n := strings.Count(out, "· n\n"); n != 8 {
		t.Errorf("%d notes, want 8: %q", n, out)
	}
	if strings.LastIndex(out, hideCursor) > strings.LastIndex(out, showCursor) {
		t.Errorf("the cursor stays hidden: %q", out)
	}
}

// The ticker goroutine writes nothing once Finish returns, and a second
// start is ignored.
func TestChecklistWritesNothingAfterFinish(t *testing.T) {
	t.Parallel()
	var b syncBuffer
	c := newChecklist(&b, styleOff(), time.Now, func() int { return 80 })
	c.start()
	c.start() // ignored: one ticker only
	c.Step("Waiting for CI")
	time.Sleep(150 * time.Millisecond) // let it tick
	c.Finish(false)
	at := b.String()
	time.Sleep(300 * time.Millisecond)
	if got := b.String(); got != at {
		t.Fatalf("written after Finish: %q", strings.TrimPrefix(got, at))
	}
	if !strings.HasSuffix(at, showCursor) {
		t.Fatalf("%q", at)
	}
}

// start after Finish starts nothing.
func TestChecklistStartAfterFinishDoesNothing(t *testing.T) {
	t.Parallel()
	var b syncBuffer
	c := newChecklist(&b, styleOff(), time.Now, func() int { return 80 })
	c.Finish(false)
	c.start()
	if c.stop != nil {
		t.Fatal("a ticker started after Finish")
	}
}

// R16: a quiet run that fails still shows the step it failed on.
func TestQuietChecklistFinishFailedPrintsTheCross(t *testing.T) {
	t.Parallel()
	w := 80
	c, b, _ := testChecklist(&w)
	c.setQuiet(true)
	c.Step("Reading acme/widget")
	c.Finish(true)
	if !strings.HasSuffix(b.String(), clearLine+"✗ Reading acme/widget\n"+showCursor) {
		t.Fatalf("%q", b.String())
	}
}

// A quiet step ended by the next one doesn't show and hide the cursor in
// between: the next step is drawn over it in place.
func TestQuietChecklistNextStepDoesNotFlickerTheCursor(t *testing.T) {
	t.Parallel()
	w := 80
	c, b, _ := testChecklist(&w)
	c.setQuiet(true)
	c.Step("Reading")
	b.Reset()
	c.Step("Surveying")
	if b.String() != clearLine+"⠋ Surveying" {
		t.Fatalf("%q", b.String())
	}
}

// A one-column terminal has no room for a line: not even the mark is drawn.
func TestMarkedIsEmptyWhenNothingFits(t *testing.T) {
	t.Parallel()
	w := 1
	c, _, _ := testChecklist(&w)
	if got := c.marked("✓", style.New(true, false).OK, "text", ""); got != "" {
		t.Fatalf("%q", got)
	}
}
