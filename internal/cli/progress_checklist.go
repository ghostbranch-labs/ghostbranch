package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli/style"
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
)

// uiProgress is progress as the CLI draws it: the engine's Progress, plus
// what the app needs around prompts and before the result.
type uiProgress interface {
	progress.Progress
	// Pause clears the spinner line and stops redrawing, before anything
	// reads the terminal; Resume draws it again. They nest.
	Pause()
	Resume()
	// Finish stops for good: the running step becomes a ✗ line (failed) or
	// an unmarked one, the spinner line is cleared and the cursor shown.
	// Later steps and notes print as plain lines. Calling it again does
	// nothing.
	Finish(failed bool)
}

const (
	clearLine  = "\r\x1b[2K"
	hideCursor = "\x1b[?25l"
	showCursor = "\x1b[?25h"
	// timerColumn is where a line's timer ends when the terminal is wider.
	timerColumn = 60
)

var spinFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// checklist draws progress on a terminal (spec 6.4a § 2.2): finished steps
// as ✓ or ✗ lines that stay, notes above, and the running step as one
// spinning line with a timer. Only the spinning line is redrawn.
type checklist struct {
	mu     sync.Mutex
	w      io.Writer
	st     style.Styler
	now    func() time.Time
	width  func() int
	frame  int
	cur    *checkStep // the running step; nil when none
	quiet  bool       // spinner only: finished steps leave no line, ✗ still does (R16)
	paused int
	shown  bool // the spinner line is on screen and the cursor hidden
	done   bool // Finish ran
	stop   chan struct{}
}

type checkStep struct {
	c     *checklist
	text  string
	start time.Time
	ended bool
}

func newChecklist(w io.Writer, st style.Styler, now func() time.Time, width func() int) *checklist {
	return &checklist{w: w, st: st, now: now, width: width}
}

// start redraws the spinner about ten times a second until Finish. It
// starts one ticker at most, and none once Finish has run.
func (c *checklist) start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done || c.stop != nil {
		return
	}
	stop := make(chan struct{})
	c.stop = stop
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				c.tick()
			}
		}
	}()
}

// tick advances the spinner one frame and redraws it.
func (c *checklist) tick() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frame++
	c.redraw()
}

func (c *checklist) cols() int {
	if c.width == nil {
		return 80
	}
	return c.width() // re-read on every draw: the window may be resized
}

func (c *checklist) write(s string) { _, _ = io.WriteString(c.w, s) }

// redraw puts the running step's line on screen; c.mu is held.
func (c *checklist) redraw() {
	if c.cur == nil || c.paused > 0 || c.done {
		return
	}
	out := clearLine
	if !c.shown {
		out = hideCursor + out
		c.shown = true
	}
	mark := string(spinFrames[c.frame%len(spinFrames)])
	c.write(out + fit(mark, c.cur.text, elapsed(c.now().Sub(c.cur.start)), c.cols()))
}

// clear removes the spinner line and shows the cursor; c.mu is held.
func (c *checklist) clear() {
	if !c.shown {
		return
	}
	c.write(clearLine + showCursor)
	c.shown = false
}

// line prints a line that stays, above the spinner; c.mu is held.
func (c *checklist) line(s string) {
	if c.shown {
		c.write(clearLine)
	}
	c.write(s + "\n")
	if c.cur == nil && c.shown {
		c.write(showCursor)
		c.shown = false
		return
	}
	c.redraw()
}

// marked lays out a line and colours only its mark; "" when nothing fits.
func (c *checklist) marked(mark string, color func(string) string, text, timer string) string {
	l := fit(mark, text, timer, c.cols())
	if l == "" {
		return ""
	}
	return color(mark) + strings.TrimPrefix(l, mark)
}

// end finishes s with mark; c.mu is held.
func (c *checklist) end(s *checkStep, mark string, color func(string) string, text string) {
	if s.ended {
		return
	}
	s.ended = true
	if text != "" {
		s.text = text
	}
	if c.cur == s {
		c.cur = nil
	}
	if c.quiet && mark != "✗" {
		// Spinner only: a finished step leaves no trace. Clear its line now,
		// or its text would stay on screen until the next draw.
		if c.cur == nil {
			c.clear()
		} else {
			c.redraw()
		}
		return
	}
	c.line(c.marked(mark, color, s.text, elapsed(c.now().Sub(s.start))))
}

// setQuiet switches spinner-only mode on or off (R16): the spinner shows
// while a step runs, finished steps leave no line, a ✗ and notes still do.
func (c *checklist) setQuiet(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.quiet = on
}

func (c *checklist) Step(text string) progress.Step {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &checkStep{c: c, text: text, start: c.now()}
	if c.done {
		s.ended = true
		c.write(flat(text) + "\n")
		return s
	}
	prev := c.cur
	c.cur = s
	if prev != nil {
		// The new step is already current, so ending the last one draws it
		// in place: in spinner-only mode the cursor isn't shown and hidden
		// again in between.
		c.end(prev, "✓", c.st.OK, "")
	} else {
		c.redraw()
	}
	return s
}

func (c *checklist) Note(level progress.NoteLevel, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		c.write(flat(text) + "\n")
		return
	}
	mark, color := "·", c.st.Dim
	if level == progress.Warn {
		mark, color = "!", c.st.Warn
	}
	c.line(c.marked(mark, color, text, ""))
}

func (s *checkStep) Update(text string) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if s.ended {
		return
	}
	s.text = text
	s.c.redraw()
}

func (s *checkStep) Done(text string) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	s.c.end(s, "✓", s.c.st.OK, text)
}

func (s *checkStep) Fail() {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	s.c.end(s, "✗", s.c.st.Fail, "")
}

func (c *checklist) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paused++
	c.clear()
}

func (c *checklist) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused > 0 {
		c.paused--
	}
	c.redraw()
}

func (c *checklist) Finish(failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return
	}
	c.paused = 0
	if c.cur != nil {
		if failed {
			c.end(c.cur, "✗", c.st.Fail, "")
		} else {
			c.end(c.cur, " ", func(s string) string { return s }, "")
		}
	}
	c.clear()
	c.done = true
	if c.stop != nil {
		close(c.stop)
	}
}

// elapsed is a step's timer: nothing under a second, then 2s, 1m12s, 1h02m.
func elapsed(d time.Duration) string {
	switch {
	case d < time.Second:
		return ""
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	default:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
}

// flat replaces control characters with spaces, so a step's text can't move
// the cursor or break the line.
func flat(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s)
}

// fit lays out one line: the mark, a space, the text, and the timer ending
// at timerColumn (or the line's end on a narrower terminal). The whole line
// is at most cols-1 runes, so it never reaches the last column, which wraps
// on some terminals; the text is cut with … and the timer is dropped when it
// doesn't fit. cols <= 0 means 80. Widths count runes (R14).
func fit(mark, text, timer string, cols int) string {
	if cols <= 0 {
		cols = 80
	}
	limit := cols - 1
	head := mark + " "
	t := []rune(flat(text))
	tail := ""
	if timer != "" {
		tail = "  " + timer
	}
	room := limit - utf8.RuneCountInString(head) - utf8.RuneCountInString(tail)
	if tail != "" && room < len(t) && room < 8 {
		tail, room = "", limit-utf8.RuneCountInString(head) // too narrow: the timer goes first
	}
	if room < 1 {
		h := []rune(head)
		if limit < 1 {
			return ""
		}
		if limit < len(h) {
			return string(h[:limit])
		}
		return head
	}
	if len(t) > room {
		t = append(t[:room-1], '…')
	}
	body := head + string(t)
	if tail == "" {
		return body
	}
	end := min(limit, timerColumn)
	if pad := end - utf8.RuneCountInString(body) - utf8.RuneCountInString(tail); pad > 0 {
		body += strings.Repeat(" ", pad)
	}
	return body + tail
}
