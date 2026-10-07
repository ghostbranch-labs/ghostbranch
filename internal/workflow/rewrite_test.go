package workflow

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func rewriteOK(t *testing.T, src, want string) {
	t.Helper()
	out, guards, err := Rewrite("w.yml", []byte(src), names)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	for _, g := range guards {
		if !g.Rewritable {
			t.Fatalf("guard %+v not rewritable", g)
		}
	}
}

const widened = `(github.repository == 'acme/widget' || github.repository == 'acme/widget-shadow')`

func TestRewriteStyles(t *testing.T) {
	head := "on: push\njobs:\n  a:\n    runs-on: x\n"
	// plain
	rewriteOK(t, head+"    if: github.repository == 'acme/widget'   # only upstream\n    steps: []\n",
		head+"    if: "+widened+"   # only upstream\n    steps: []\n")
	// ${{ }} plain
	rewriteOK(t, head+"    if: ${{ github.repository == 'acme/widget' }}\n",
		head+"    if: ${{ "+widened+" }}\n")
	// double-quoted
	rewriteOK(t, head+"    if: \"github.repository == 'acme/widget'\"\n",
		head+"    if: \""+widened+"\"\n")
	// single-quoted, with '' escapes
	rewriteOK(t, head+"    if: 'github.repository == ''acme/widget'''\n",
		head+"    if: '"+strings.ReplaceAll(widened, "'", "''")+"'\n")
	// literal block: re-emitted double-quoted, the trailing newline kept
	rewriteOK(t, head+"    if: |\n      github.repository == 'acme/widget'\n    steps: []\n",
		head+"    if: \""+widened+"\\n\"\n    steps: []\n")
	// folded block with strip chomping
	rewriteOK(t, head+"    if: >-\n      github.repository == 'acme/widget'\n",
		head+"    if: \""+widened+"\"\n")
}

// Review Focus 1.
func TestRewriteKeepsCRLF(t *testing.T) {
	src := "on: push\r\njobs:\r\n  a:\r\n    if: github.repository == 'acme/widget'\r\n    runs-on: x\r\n"
	want := "on: push\r\njobs:\r\n  a:\r\n    if: " + widened + "\r\n    runs-on: x\r\n"
	rewriteOK(t, src, want)
}

// Review Focus 2.
func TestRewriteInlineSequenceItemAndUnicode(t *testing.T) {
	src := "on: push\njobs:\n  a:\n    runs-on: x\n    steps:\n      - name: déployer 🚀\n        run: echo é\n      - if: github.repository == 'acme/widget'\n        run: echo hi\n"
	want := "on: push\njobs:\n  a:\n    runs-on: x\n    steps:\n      - name: déployer 🚀\n        run: echo é\n      - if: " + widened + "\n        run: echo hi\n"
	rewriteOK(t, src, want)
}

func TestRewriteLeavesOtherBytesAlone(t *testing.T) {
	src := []byte(sample)
	out, guards, err := Rewrite(".github/workflows/ci.yml", src, names)
	if err != nil {
		t.Fatal(err)
	}
	// two rewritable guards and one unrewritable
	var rw, un int
	for _, g := range guards {
		if g.Rewritable {
			rw++
		} else {
			un++
		}
	}
	if rw != 2 || un != 1 {
		t.Fatalf("guards = %+v", guards)
	}
	// every line but the two if: lines is byte-identical
	a, b := bytes.Split(src, []byte("\n")), bytes.Split(out, []byte("\n"))
	if len(a) != len(b) {
		t.Fatal("line count changed")
	}
	changed := 0
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			changed++
			if !bytes.Contains(a[i], []byte("if:")) {
				t.Fatalf("line %d changed: %q", i+1, b[i])
			}
		}
	}
	if changed != 2 {
		t.Fatalf("%d lines changed", changed)
	}
	again, _, _ := Rewrite(".github/workflows/ci.yml", src, names)
	if !bytes.Equal(again, out) {
		t.Fatal("Rewrite is not deterministic")
	}
}

// rewriteRefused checks that Rewrite left src unchanged and reported every
// guard, none of them rewritable.
func rewriteRefused(t *testing.T, name, src string) {
	t.Helper()
	out, guards, err := Rewrite("w.yml", []byte(src), names)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if string(out) != src {
		t.Fatalf("%s: changed:\n%s", name, out)
	}
	if len(guards) == 0 {
		t.Fatalf("%s: no guard reported", name)
	}
	for _, g := range guards {
		if g.Rewritable {
			t.Fatalf("%s: %+v reported rewritable", name, g)
		}
	}
}

func TestRewriteRefusesWhatItCannotCheck(t *testing.T) {
	head := "on: push\njobs:\n  a:\n    runs-on: x\n"
	for name, src := range map[string]string{
		"multi-line plain":       head + "    if: github.repository ==\n      'acme/widget'\n",
		"anchored and aliased":   head + "    if: &g github.repository == 'acme/widget'\n  b:\n    runs-on: x\n    if: *g\n",
		"commented block header": head + "    if: | # only upstream\n      github.repository == 'acme/widget'\n",
	} {
		rewriteRefused(t, name, src)
	}
}

// The re-parse check is the last safety net (spec §8.2): here span finds a
// span, but a wrong one, so only the re-parse can catch it. Every guard in
// the file, the good one in job b included, falls back to unrewritable.
func TestRewriteReparseCatchesABadSpan(t *testing.T) {
	src := "on: push\njobs:\n  a:\n    runs-on: x\n    if: |2\n        github.repository == 'acme/widget'\n      && true\n" +
		"  b:\n    runs-on: x\n    if: github.repository == 'acme/widget'\n"
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	ts := targets(root(&doc))
	if len(ts) != 2 {
		t.Fatalf("targets = %+v", ts)
	}
	if _, _, _, ok := span([]byte(src), ts[0].node); !ok {
		t.Fatal("span refused: the test no longer reaches the re-parse check")
	}
	if _, guard, ok := guardExpr(ts[0].node.Value, names); !guard || !ok {
		t.Fatal("want a rewritable guard in job a")
	}
	rewriteRefused(t, "indentation indicator", src)
}

// Review Focus 2: non-ASCII text on the same line, before the if: scalar.
func TestRewriteSameLineUnicodeColumns(t *testing.T) {
	pre := "on: push\njobs:\n  a:\n    runs-on: x\n    steps:\n      - {name: déployer 🚀, if: "
	// a quoted scalar in a flow mapping: rewritten in place
	rewriteOK(t, pre+"\"github.repository == 'acme/widget'\", run: x}\n",
		pre+"\""+widened+"\", run: x}\n")
	// a plain scalar in a flow mapping: span can't tell where it ends, so it refuses
	rewriteRefused(t, "plain in flow", pre+"github.repository == 'acme/widget', run: x}\n")
}

func TestRewriteNoGuardsIsIdentity(t *testing.T) {
	src := []byte("on: push\njobs:\n  a:\n    runs-on: x\n    if: github.event_name == 'push'\n")
	out, guards, err := Rewrite("w.yml", src, names)
	if err != nil || !bytes.Equal(out, src) || len(guards) != 0 {
		t.Fatalf("%s %+v %v", out, guards, err)
	}
	if _, _, err := Rewrite("w.yml", []byte("jobs: [\n"), names); err == nil {
		t.Fatal("want the parse error")
	}
}

func TestRewriteIsIdempotent(t *testing.T) {
	once, _, err := Rewrite(".github/workflows/ci.yml", []byte(sample), names)
	if err != nil {
		t.Fatal(err)
	}
	twice, guards, err := Rewrite(".github/workflows/ci.yml", once, names)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(twice, once) {
		t.Fatalf("widened again:\n%s", twice)
	}
	// only the guard it cannot rewrite is still reported
	if len(guards) != 1 || guards[0].Rewritable {
		t.Fatalf("guards = %+v", guards)
	}
}
