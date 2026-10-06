// Package strip builds the tree promote publishes: the feature without
// overlay paths, and without the internal sections marked inside files.
// The gate's CI runs on this tree, so CI tests exactly what goes public.
package strip

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
)

// Markers are the tokens that open and close an internal section.
type Markers struct {
	Begin string
	End   string
}

// Problem is a marker that cannot be stripped safely.
type Problem struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Problem string `json:"problem"`
}

// Objects reads and writes blobs; *gitx.Git satisfies it.
type Objects interface {
	ReadBlobs(ctx context.Context, shas []string) (map[string][]byte, error)
	HashObject(ctx context.Context, content []byte) (string, error)
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// The comment wrappers a marker line may have, at most one each. Pairs are
// tried first, so "{/*" wins over "/*".
var (
	pairs    = [][2]string{{"{/*", "*/}"}, {"<!--", "-->"}, {"/*", "*/"}}
	prefixes = []string{"//", "#", "--", ";", "%"}
)

type kind int

const (
	notMarker kind = iota
	exact
	malformed
)

// unwrap removes one comment wrapper, if the line has one.
func unwrap(s string) (string, bool) {
	for _, p := range pairs {
		if len(s) >= len(p[0])+len(p[1]) && strings.HasPrefix(s, p[0]) && strings.HasSuffix(s, p[1]) {
			return strings.TrimSpace(s[len(p[0]) : len(s)-len(p[1])]), true
		}
	}
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return strings.TrimSpace(s[len(p):]), true
		}
	}
	return s, false
}

// tokenAt reports whether s starts with token, ignoring case, and returns
// what follows it.
func tokenAt(s, token string) (string, bool) {
	if len(s) < len(token) || !strings.EqualFold(s[:len(token)], token) {
		return "", false
	}
	return s[len(token):], true
}

// endsWord reports whether rest continues the token as a longer word: it
// starts with an ASCII letter or digit, '-' or '_'. Anything else, including
// the end of the line, ends the token.
func endsWord(rest string) bool {
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		return false
	}
	return true
}

// classify says whether a line is exactly token (ignoring case), after
// whitespace and at most one comment wrapper. A line that is not exact but
// looks meant as a marker is malformed, because guessing wrong would publish
// the internal lines: a comment that starts with the token and then has
// anything but more word characters, or a near miss (see nearMiss).
func classify(line, token string) kind {
	s, wrapped := unwrap(strings.TrimSpace(line))
	rest, ok := tokenAt(s, token)
	switch {
	case ok && rest == "":
		return exact
	case wrapped && ok && endsWord(rest):
		return malformed
	case nearMiss(strings.TrimSpace(line), token):
		return malformed
	}
	return notMarker
}

// The comment punctuation a near miss may start with, and the comment
// closers it may end with.
const leadPunct = "/*#-;%<!{ \t"

var closers = []string{"*/}", "-->", "*/"}

// nearMiss reports a line that, once any leading run of comment punctuation
// and whitespace and any trailing comment closers are removed, starts with
// token (ignoring case) followed by the end of the line or any character
// but an ASCII letter or digit, '-' or '_'. That catches a
// marker on the first or last line of a multi-line comment, and doubled
// prefixes such as "///" or "##". Text before the token, or a longer token,
// is not a near miss.
func nearMiss(s, token string) bool {
	for cut := true; cut; {
		cut = false
		s = strings.TrimRight(s, " \t")
		for _, c := range closers {
			if strings.HasSuffix(s, c) {
				s, cut = s[:len(s)-len(c)], true
				break
			}
		}
	}
	// A token may itself start with punctuation, so try it at every point
	// of the leading run rather than only after it.
	for i := 0; i <= len(s); i++ {
		if rest, ok := tokenAt(s[i:], token); ok && endsWord(rest) {
			return true
		}
		if i == len(s) || !strings.ContainsRune(leadPunct, rune(s[i])) {
			return false
		}
	}
	return false
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			return append(out, b)
		}
		out = append(out, b[:i+1])
		b = b[i+1:]
	}
	return out
}

// Cut removes every internal section from one blob: whole lines, from a
// begin marker line through its end marker line, line endings included.
// Every other byte is kept, a UTF-8 BOM included. Binaries pass through.
// Markers in UTF-16 or UTF-32 text, or in printable runs of a blob read as
// binary, are a problem, because the strip cannot re-encode those
// safely. On a problem the input comes back unchanged.
func Cut(b []byte, m Markers) ([]byte, bool, *Problem) {
	text, enc := leak.Decode(b)
	switch enc {
	case leak.Binary:
		// A binary is never edited, but text the decoder could not read
		// (BOM-less UTF-16 that is mostly non-Latin, or a stray NUL) still
		// shows its markers as printable runs.
		for _, run := range strings.Split(leak.Runs(b), "\n") {
			if classify(run, m.Begin) != notMarker || classify(run, m.End) != notMarker {
				return b, false, &Problem{Line: 1, Problem: "unsupported_encoding"}
			}
		}
		return b, false, nil
	case leak.UTF16, leak.UTF32:
		for i, ln := range strings.Split(text, "\n") {
			if classify(ln, m.Begin) != notMarker || classify(ln, m.End) != notMarker {
				return b, false, &Problem{Line: i + 1, Problem: "unsupported_encoding"}
			}
		}
		return b, false, nil
	}
	body := b
	var bom []byte
	if bytes.HasPrefix(b, utf8BOM) {
		bom, body = b[:3], b[3:]
	}
	var out []byte
	open, changed := 0, false
	for i, ln := range splitLines(body) {
		n := i + 1
		content := strings.TrimRight(string(ln), "\r\n")
		begin, end := classify(content, m.Begin), classify(content, m.End)
		switch {
		case begin == malformed || end == malformed:
			return b, false, &Problem{Line: n, Problem: "malformed"}
		case begin == exact:
			if open != 0 {
				return b, false, &Problem{Line: n, Problem: "nested"}
			}
			open, changed = n, true
		case end == exact:
			if open == 0 {
				return b, false, &Problem{Line: n, Problem: "unmatched_end"}
			}
			open = 0
		case open == 0:
			out = append(out, ln...)
		}
	}
	if open != 0 {
		return b, false, &Problem{Line: open, Problem: "unmatched_begin"}
	}
	if !changed {
		return b, false, nil
	}
	return append(append([]byte{}, bom...), out...), true, nil
}

// editable is a regular file: symlinks (120000) and submodules are never
// edited.
func editable(en gitx.TreeEntry) bool { return en.Type == "blob" && en.Mode != "120000" }

// Tree returns entries without overlay paths, with internal sections cut
// out of every editable blob. Blobs without markers keep their SHA. Every
// bad file is reported at once as MARKER_INVALID.
func Tree(ctx context.Context, objs Objects, entries []gitx.TreeEntry, set overlay.Set, m Markers) ([]gitx.TreeEntry, error) {
	keep := make([]gitx.TreeEntry, 0, len(entries))
	var shas []string
	seen := map[string]bool{}
	for _, en := range entries {
		if set.Match(en.Path) {
			continue
		}
		keep = append(keep, en)
		if editable(en) && !seen[en.SHA] {
			seen[en.SHA] = true
			shas = append(shas, en.SHA)
		}
	}
	blobs, err := objs.ReadBlobs(ctx, shas)
	if err != nil {
		return nil, contract.Wrap(contract.Internal, err, "read files to strip")
	}
	var problems []Problem
	written := map[string]string{} // original SHA -> stripped SHA
	for i, en := range keep {
		if !editable(en) {
			continue
		}
		out, changed, p := Cut(blobs[en.SHA], m)
		if p != nil {
			p.Path = en.Path
			problems = append(problems, *p)
			continue
		}
		if !changed {
			continue
		}
		sha, ok := written[en.SHA]
		if !ok {
			if sha, err = objs.HashObject(ctx, out); err != nil {
				return nil, contract.Wrap(contract.Internal, err, "write a stripped file")
			}
			written[en.SHA] = sha
		}
		keep[i].SHA = sha
	}
	if len(problems) > 0 {
		var parts []string
		for _, p := range problems {
			parts = append(parts, fmt.Sprintf("%s:%d %s", p.Path, p.Line, p.Problem))
		}
		return nil, contract.New(contract.MarkerInvalid, "%s/%s markers cannot be stripped safely: %s",
			m.Begin, m.End, strings.Join(parts, ", ")).WithDetails(map[string]any{"problems": problems})
	}
	return keep, nil
}
