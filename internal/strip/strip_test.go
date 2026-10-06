package strip_test

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/strip"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

var markers = strip.Markers{Begin: "BEGIN-INTERNAL", End: "END-INTERNAL"}

// Review Focus 2 and 3.
func TestCut(t *testing.T) {
	cases := []struct {
		name, in, want string
		problem        string
		line           int
	}{
		{name: "slash comments", in: "a\n// BEGIN-INTERNAL\nsecret\n// END-INTERNAL\nb\n", want: "a\nb\n"},
		{name: "hash", in: "a\n# BEGIN-INTERNAL\nx\n# END-INTERNAL\n", want: "a\n"},
		{name: "sql dashes", in: "-- BEGIN-INTERNAL\nx\n-- END-INTERNAL\nb\n", want: "b\n"},
		{name: "semicolon", in: "; BEGIN-INTERNAL\nx\n; END-INTERNAL\n", want: ""},
		{name: "percent, no space", in: "%BEGIN-INTERNAL\nx\n%END-INTERNAL\nb", want: "b"},
		{name: "indented block comment", in: "  /* BEGIN-INTERNAL */\nx\n  /* END-INTERNAL */\n", want: ""},
		{name: "html", in: "<p>a</p>\n<!-- BEGIN-INTERNAL -->\n<p>x</p>\n<!-- END-INTERNAL -->\n", want: "<p>a</p>\n"},
		{name: "jsx", in: "{/* BEGIN-INTERNAL */}\n<X/>\n{/* END-INTERNAL */}\nok\n", want: "ok\n"},
		{name: "bare tokens", in: "BEGIN-INTERNAL\nx\nEND-INTERNAL\n", want: ""},
		{name: "crlf kept", in: "a\r\n// BEGIN-INTERNAL\r\nx\r\n// END-INTERNAL\r\nb\r\n", want: "a\r\nb\r\n"},
		{name: "mixed endings kept", in: "a\n// BEGIN-INTERNAL\r\nx\n// END-INTERNAL\nb\r\nc", want: "a\nb\r\nc"},
		{name: "end marker without newline", in: "a\n// BEGIN-INTERNAL\nx\n// END-INTERNAL", want: "a\n"},
		{name: "two sections", in: "a\n#BEGIN-INTERNAL\nx\n#END-INTERNAL\nb\n#BEGIN-INTERNAL\ny\n#END-INTERNAL\nc\n", want: "a\nb\nc\n"},
		{name: "only internal: empty file", in: "// BEGIN-INTERNAL\nx\n// END-INTERNAL\n", want: ""},
		{name: "utf-8 bom kept", in: "\xEF\xBB\xBF// BEGIN-INTERNAL\nx\n// END-INTERNAL\ny\n", want: "\xEF\xBB\xBFy\n"},
		{name: "latin-1 bytes kept", in: "caf\xe9\n# BEGIN-INTERNAL\nx\n# END-INTERNAL\n", want: "caf\xe9\n"},
		{name: "prose is not a marker", in: "Code between BEGIN-INTERNAL and END-INTERNAL is removed.\n", want: "Code between BEGIN-INTERNAL and END-INTERNAL is removed.\n"},
		{name: "string literal is not a marker", in: "s := \"// BEGIN-INTERNAL\"\n", want: "s := \"// BEGIN-INTERNAL\"\n"},
		{name: "prose after a comment is not a marker", in: "// see BEGIN-INTERNAL below\n", want: "// see BEGIN-INTERNAL below\n"},
		{name: "prose line starting with a word", in: "Use BEGIN-INTERNAL: to open\n", want: "Use BEGIN-INTERNAL: to open\n"},
		{name: "a longer token is not a marker", in: "// BEGIN-INTERNALS\n", want: "// BEGIN-INTERNALS\n"},
		{name: "commented marker with more text", in: "a\n// BEGIN-INTERNAL billing hooks\nx\n// END-INTERNAL\n", problem: "malformed", line: 2},
		{name: "commented marker with a colon", in: "# END-INTERNAL: done\n", problem: "malformed", line: 1},
		{name: "multi-line block comment begin", in: "a\n/* BEGIN-INTERNAL\nx\n*/\n", problem: "malformed", line: 2},
		{name: "multi-line block comment end", in: "/*\nx\nEND-INTERNAL */\n", problem: "malformed", line: 3},
		{name: "multi-line html comment begin", in: "<p>a</p>\n<!-- BEGIN-INTERNAL\n<p>x</p>\n-->\n", problem: "malformed", line: 2},
		{name: "multi-line html comment end", in: "<!--\n<p>x</p>\nEND-INTERNAL -->\n", problem: "malformed", line: 3},
		{name: "triple slash", in: "/// BEGIN-INTERNAL\nx\n/// END-INTERNAL\n", problem: "malformed", line: 1},
		{name: "double hash", in: "a\n## BEGIN-INTERNAL\nx\n## END-INTERNAL\n", problem: "malformed", line: 2},
		{name: "doubled block comment opener", in: "/** END-INTERNAL */\n", problem: "malformed", line: 1},
		{name: "unmatched begin", in: "a\n// BEGIN-INTERNAL\nx\n", problem: "unmatched_begin", line: 2},
		{name: "unmatched end", in: "a\n// END-INTERNAL\n", problem: "unmatched_end", line: 2},
		{name: "nested", in: "// BEGIN-INTERNAL\n// BEGIN-INTERNAL\n// END-INTERNAL\n// END-INTERNAL\n", problem: "nested", line: 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, changed, p := strip.Cut([]byte(c.in), markers)
			if c.problem != "" {
				if p == nil || p.Problem != c.problem || p.Line != c.line || changed || string(out) != c.in {
					t.Fatalf("Cut = %q, %v, %+v; want problem %s at line %d and the input back", out, changed, p, c.problem, c.line)
				}
				return
			}
			if p != nil || string(out) != c.want || changed != (c.want != c.in) {
				t.Fatalf("Cut = %q, %v, %+v; want %q", out, changed, p, c.want)
			}
		})
	}
}

func TestCutLeavesOtherEncodingsAlone(t *testing.T) {
	var marked []byte
	for _, u := range utf16.Encode([]rune("// BEGIN-INTERNAL\nx\n// END-INTERNAL\n")) {
		marked = append(marked, byte(u), byte(u>>8))
	}
	withBOM := append([]byte{0xFF, 0xFE}, marked...)
	if _, _, p := strip.Cut(withBOM, markers); p == nil || p.Problem != "unsupported_encoding" || p.Line != 1 {
		t.Fatalf("markers in UTF-16 must fail, got %+v", p)
	}
	plain := append([]byte{0xFF, 0xFE}, 'h', 0, 'i', 0)
	if out, changed, p := strip.Cut(plain, markers); p != nil || changed || string(out) != string(plain) {
		t.Fatal("UTF-16 without markers must pass through")
	}
	bin := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01 plain image bytes\n")
	if out, changed, p := strip.Cut(bin, markers); p != nil || changed || string(out) != string(bin) {
		t.Fatal("binaries without markers pass through unchanged")
	}
}

// A file the decoder reads as binary is never edited, so markers in it
// must fail rather than publish the internal lines.
func TestCutRefusesMarkersInBinaries(t *testing.T) {
	// BOM-less UTF-16 that is mostly CJK has too few NULs to be detected.
	var cjk []byte
	for _, u := range utf16.Encode([]rune("// BEGIN-INTERNAL\n" + strings.Repeat("内部の秘密", 20) + "\n// END-INTERNAL\n" + strings.Repeat("公開する文書", 20) + "\n")) {
		cjk = append(cjk, byte(u), byte(u>>8))
	}
	strayNUL := []byte("a\x00b\n// BEGIN-INTERNAL\nsecret\n// END-INTERNAL\nc\n")
	for name, b := range map[string][]byte{"utf-16 cjk": cjk, "stray nul": strayNUL} {
		t.Run(name, func(t *testing.T) {
			out, changed, p := strip.Cut(b, markers)
			if p == nil || p.Problem != "unsupported_encoding" || p.Line != 1 || changed || string(out) != string(b) {
				t.Fatalf("Cut = %v, %+v; want unsupported_encoding at line 1 and the input back", changed, p)
			}
		})
	}
}

func TestCustomTokens(t *testing.T) {
	out, changed, p := strip.Cut([]byte("a\n// @@private\nx\n// @@end\n"), strip.Markers{Begin: "@@private", End: "@@end"})
	if p != nil || !changed || string(out) != "a\n" {
		t.Fatalf("Cut = %q %v %+v", out, changed, p)
	}
}

func TestTree(t *testing.T) {
	ctx := context.Background()
	g := testutil.BareRepo(t, filepath.Join(t.TempDir(), "r.git"))
	blob := func(s string) string {
		t.Helper()
		sha, err := g.HashObject(ctx, []byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}
	marked := blob("a\n// BEGIN-INTERNAL\nx\n// END-INTERNAL\n")
	plain := blob("plain\n")
	sub := "0123456789012345678901234567890123456789"
	entries := []gitx.TreeEntry{
		{Mode: "100644", Type: "blob", SHA: blob("cfg"), Path: ".ghostbranch.yml"},
		{Mode: "100644", Type: "blob", SHA: blob("n"), Path: "docs/private.md"},
		{Mode: "100644", Type: "blob", SHA: marked, Path: "a.go"},
		{Mode: "100755", Type: "blob", SHA: marked, Path: "bin/run.sh"},
		{Mode: "120000", Type: "blob", SHA: marked, Path: "link"},
		{Mode: "100644", Type: "blob", SHA: plain, Path: "plain.txt"},
		{Mode: "160000", Type: "commit", SHA: sub, Path: "vendor/sub"},
	}
	got, err := strip.Tree(ctx, g, entries, overlay.NewSet("docs/private.md"), markers)
	if err != nil {
		t.Fatal(err)
	}
	stripped := blob("a\n")
	want := []gitx.TreeEntry{
		{Mode: "100644", Type: "blob", SHA: stripped, Path: "a.go"},
		{Mode: "100755", Type: "blob", SHA: stripped, Path: "bin/run.sh"},
		{Mode: "120000", Type: "blob", SHA: marked, Path: "link"},
		{Mode: "100644", Type: "blob", SHA: plain, Path: "plain.txt"},
		{Mode: "160000", Type: "commit", SHA: sub, Path: "vendor/sub"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tree =\n%+v\nwant\n%+v", got, want)
	}
}

func TestTreeReportsEveryBadFile(t *testing.T) {
	ctx := context.Background()
	g := testutil.BareRepo(t, filepath.Join(t.TempDir(), "r.git"))
	open, _ := g.HashObject(ctx, []byte("// BEGIN-INTERNAL\n"))
	closed, _ := g.HashObject(ctx, []byte("x\n// END-INTERNAL\n"))
	_, err := strip.Tree(ctx, g, []gitx.TreeEntry{
		{Mode: "100644", Type: "blob", SHA: open, Path: "a.go"},
		{Mode: "100644", Type: "blob", SHA: closed, Path: "b.md"},
	}, overlay.NewSet(), markers)
	ce := contract.From(err)
	if ce == nil || ce.Code != contract.MarkerInvalid {
		t.Fatalf("want MARKER_INVALID, got %v", err)
	}
	want := []strip.Problem{{Path: "a.go", Line: 1, Problem: "unmatched_begin"}, {Path: "b.md", Line: 2, Problem: "unmatched_end"}}
	if !reflect.DeepEqual(ce.Details["problems"], want) {
		t.Fatalf("problems = %+v", ce.Details["problems"])
	}
}
