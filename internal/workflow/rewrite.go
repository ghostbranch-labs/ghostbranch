package workflow

import (
	"bytes"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

type edit struct {
	t          target
	value      string
	start, end int
	text       string
}

// Rewrite widens every rewritable guard in src so it also matches the
// shadow repo. Only the bytes of those if: values change. The result is
// parsed again: unless every other value is unchanged and each if: reads
// exactly as intended, Rewrite returns src and reports every guard
// unrewritable. The same src and names always give the same bytes. The
// error is only ever src's YAML parse error.
func Rewrite(path string, src []byte, n Names) ([]byte, []Guard, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return src, nil, err
	}
	r := root(&doc)
	if r == nil {
		return src, nil, nil
	}
	var guards []Guard
	var edits []edit
	for _, t := range targets(r) {
		out, guard, ok := guardExpr(t.node.Value, n)
		if !guard {
			continue
		}
		guards = append(guards, Guard{Path: path, Job: t.job, Step: t.name, Expr: strings.TrimSpace(t.node.Value), Rewritable: ok})
		if ok {
			edits = append(edits, edit{t: t, value: out})
		}
	}
	if len(edits) == 0 {
		return src, guards, nil
	}
	fail := func() ([]byte, []Guard, error) {
		for i := range guards {
			guards[i].Rewritable = false
		}
		return src, guards, nil
	}
	for i := range edits {
		s, e, single, ok := span(src, edits[i].t.node)
		if !ok {
			return fail()
		}
		edits[i].start, edits[i].end = s, e
		edits[i].text = emit(edits[i].t.node.Style, single, edits[i].value)
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := src
	limit := len(src)
	for _, ed := range edits {
		if ed.end > limit {
			return fail() // overlapping spans
		}
		var b bytes.Buffer
		b.Write(out[:ed.start])
		b.WriteString(ed.text)
		b.Write(out[ed.end:])
		out = b.Bytes()
		limit = ed.start
	}
	if !sameExceptIfs(src, out, edits) {
		return fail()
	}
	return out, guards, nil
}

// sameExceptIfs parses both documents and checks they are equal once the
// edited if: values are set to their new text in the original.
func sameExceptIfs(src, out []byte, edits []edit) bool {
	var before, after any
	if yaml.Unmarshal(src, &before) != nil || yaml.Unmarshal(out, &after) != nil {
		return false
	}
	for _, ed := range edits {
		if !setIf(before, ed.t.job, ed.t.step, ed.value) {
			return false
		}
	}
	return reflect.DeepEqual(before, after)
}

func setIf(doc any, job string, step int, v string) bool {
	m, ok := doc.(map[string]any)
	if !ok {
		return false
	}
	jobs, ok := m["jobs"].(map[string]any)
	if !ok {
		return false
	}
	j, ok := jobs[job].(map[string]any)
	if !ok {
		return false
	}
	if step < 0 {
		j["if"] = v
		return true
	}
	steps, ok := j["steps"].([]any)
	if !ok || step >= len(steps) {
		return false
	}
	s, ok := steps[step].(map[string]any)
	if !ok {
		return false
	}
	s["if"] = v
	return true
}

// lineEnd is the end of the line holding i, before any \r\n or \n.
func lineEnd(src []byte, i int) int {
	e := len(src)
	if j := bytes.IndexByte(src[i:], '\n'); j >= 0 {
		e = i + j
	}
	if e > i && src[e-1] == '\r' {
		e--
	}
	return e
}

// nextLine is the start of the line after the one ending at le.
func nextLine(src []byte, le int) int {
	if le < len(src) && src[le] == '\r' {
		le++
	}
	if le < len(src) && src[le] == '\n' {
		le++
	}
	return le
}

// offset turns yaml.v3's 1-based line and column (in characters) into a
// byte offset.
func offset(src []byte, line, col int) (int, bool) {
	i := 0
	for l := 1; l < line; l++ {
		j := bytes.IndexByte(src[i:], '\n')
		if j < 0 {
			return 0, false
		}
		i += j + 1
	}
	for c := 1; c < col; c++ {
		if i >= len(src) || src[i] == '\n' {
			return 0, false
		}
		_, size := utf8.DecodeRune(src[i:])
		i += size
	}
	return i, true
}

func indentOf(line []byte) int { return len(line) - len(bytes.TrimLeft(line, " ")) }

// span finds the bytes of a scalar node in src. single reports a one-line
// flow scalar.
func span(src []byte, nd *yaml.Node) (start, end int, single, ok bool) {
	start, ok = offset(src, nd.Line, nd.Column)
	if !ok || start >= len(src) {
		return 0, 0, false, false
	}
	switch nd.Style {
	case yaml.DoubleQuotedStyle:
		if src[start] != '"' {
			return 0, 0, false, false
		}
		for i := start + 1; i < len(src); i++ {
			switch src[i] {
			case '\\':
				i++
			case '"':
				return start, i + 1, !bytes.ContainsAny(src[start:i], "\r\n"), true
			}
		}
	case yaml.SingleQuotedStyle:
		if src[start] != '\'' {
			return 0, 0, false, false
		}
		for i := start + 1; i < len(src); i++ {
			if src[i] != '\'' {
				continue
			}
			if i+1 < len(src) && src[i+1] == '\'' {
				i++
				continue
			}
			return start, i + 1, !bytes.ContainsAny(src[start:i], "\r\n"), true
		}
	case yaml.LiteralStyle, yaml.FoldedStyle:
		if src[start] != '|' && src[start] != '>' {
			return 0, 0, false, false
		}
		return blockSpan(src, start)
	case 0:
		text := src[start:lineEnd(src, start)]
		if i := bytes.Index(text, []byte(" #")); i >= 0 {
			text = text[:i]
		}
		if i := bytes.Index(text, []byte("\t#")); i >= 0 {
			text = text[:i]
		}
		text = bytes.TrimRight(text, " \t")
		if string(text) != nd.Value {
			return 0, 0, false, false // a multi-line plain scalar
		}
		return start, start + len(text), true, true
	}
	return 0, 0, false, false
}

// blockSpan is a block scalar from its indicator to the end of its last
// content line (line break excluded). It refuses a header that holds a
// comment, since the re-parse check can't see a dropped comment.
func blockSpan(src []byte, start int) (int, int, bool, bool) {
	ls := bytes.LastIndexByte(src[:start], '\n') + 1
	keyIndent := indentOf(src[ls:lineEnd(src, ls)])
	hdrEnd := lineEnd(src, start)
	if bytes.IndexByte(src[start:hdrEnd], '#') >= 0 {
		return 0, 0, false, false // a comment the rewrite would drop
	}
	end, indent := hdrEnd, -1
	for i := nextLine(src, hdrEnd); i < len(src); {
		le := lineEnd(src, i)
		line := src[i:le]
		if len(bytes.TrimSpace(line)) == 0 {
			i = nextLine(src, le)
			continue
		}
		ind := indentOf(line)
		if indent < 0 {
			if ind <= keyIndent {
				break
			}
			indent = ind
		}
		if ind < indent {
			break
		}
		end = le
		i = nextLine(src, le)
	}
	if end == hdrEnd {
		return 0, 0, false, false
	}
	return start, end, false, true
}

// emit writes v as a YAML scalar, keeping the original style when it can
// hold v safely, and otherwise double-quoted.
func emit(style yaml.Style, single bool, v string) string {
	switch {
	case style == 0 && single && plainSafe(v):
		return v
	case style == yaml.SingleQuotedStyle && single && !strings.ContainsAny(v, "\r\n"):
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	return strconv.Quote(v)
}

func plainSafe(v string) bool {
	if v == "" || strings.TrimSpace(v) != v || strings.ContainsAny(v, "\r\n\t") ||
		strings.Contains(v, ": ") || strings.Contains(v, " #") || strings.HasSuffix(v, ":") {
		return false
	}
	return !strings.ContainsRune("-?:,[]{}#&*!|>'\"%@`", rune(v[0]))
}
