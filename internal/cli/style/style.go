// Package style adds ANSI colour to human terminal output. It is pure: the
// caller decides whether a stream gets colour (see cli.DefaultEnv), and a
// disabled Styler returns every string unchanged.
package style

// Styler wraps text in SGR escape codes. The zero value is disabled.
type Styler struct{ enabled, truecolor bool }

// New returns a Styler. truecolor picks 24-bit colours over the basic bright
// ANSI ones; it has no effect when enabled is false.
func New(enabled, truecolor bool) Styler { return Styler{enabled, truecolor} }

const reset = "\x1b[0m"

func (s Styler) wrap(code, text string) string {
	if !s.enabled {
		return text
	}
	return "\x1b[" + code + "m" + text + reset
}

func (s Styler) color(basic, rgb, text string) string {
	if s.truecolor {
		return s.wrap("38;2;"+rgb, text)
	}
	return s.wrap(basic, text)
}

// OK is lime green.
func (s Styler) OK(text string) string { return s.color("92", "132;204;22", text) }

// Warn is yellow.
func (s Styler) Warn(text string) string { return s.color("93", "234;179;8", text) }

// Fail is red.
func (s Styler) Fail(text string) string { return s.color("91", "239;68;68", text) }

// Dim is faint text.
func (s Styler) Dim(text string) string { return s.wrap("2", text) }

// Bold is bold text.
func (s Styler) Bold(text string) string { return s.wrap("1", text) }
