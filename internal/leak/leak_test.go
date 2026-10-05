package leak_test

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func scanner(t *testing.T) *leak.Gitleaks {
	t.Helper()
	s, err := leak.NewGitleaks(overlay.NewSet("internal/**"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCleanInputHasNoFindings(t *testing.T) {
	got, err := scanner(t).Scan(leak.Input{
		Paths: []string{"README.md"},
		Files: []leak.File{{Path: "README.md", Content: []byte("# hello\n")}},
		Texts: []leak.Text{{Where: "commit_message", Content: "Add a greeting"}},
	})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestSecretsAreFoundAndRedacted(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	got, err := scanner(t).Scan(leak.Input{
		Paths: []string{"config.env"},
		Files: []leak.File{{Path: "config.env", Content: []byte("A=1\nTOKEN=" + tok + "\n")}},
		Texts: []leak.Text{{Where: "pr_body", Content: "temporary token " + tok}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var file, text bool
	for _, f := range got {
		if f.Secret != leak.Redacted {
			t.Errorf("finding not redacted: %+v", f)
		}
		if f.Where == "tree" && f.Path == "config.env" && f.Line > 0 {
			file = true
		}
		if f.Where == "pr_body" {
			text = true
		}
	}
	if !file || !text {
		t.Fatalf("want a tree finding in config.env and a pr_body finding, got %+v", got)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), tok) || strings.Contains(string(b), tok[4:]) {
		t.Fatal("secret value leaked into findings JSON")
	}
}

func TestOverlayPathsInThePublishedTreeAreFindings(t *testing.T) {
	got, err := scanner(t).Scan(leak.Input{Paths: []string{"src/a.go", ".ghostbranch/notes.md", "internal/plan.md"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Rule != "overlay-path" || got[1].Rule != "overlay-path" {
		t.Fatalf("got %+v", got)
	}
}

func TestBinaryFilesAreSkipped(t *testing.T) {
	content := append([]byte{0x00, 0x01}, []byte(testutil.FakeGitHubToken())...)
	got, err := scanner(t).Scan(leak.Input{Paths: []string{"x.bin"}, Files: []leak.File{{Path: "x.bin", Content: content}}})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v %v", got, err)
	}
	if !strings.HasPrefix(scanner(t).Version(), "gitleaks") {
		t.Fatalf("Version = %q", scanner(t).Version())
	}
}

func TestLinesAreOneBased(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	got, err := scanner(t).Scan(leak.Input{
		Files: []leak.File{{Path: "a.env", Content: []byte("TOKEN=" + tok + "\n")}},
		Texts: []leak.Text{{Where: "pr_body", Content: "TOKEN=" + tok}},
	})
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v %v", got, err)
	}
	for _, f := range got {
		if f.Line != 1 {
			t.Errorf("finding on the first line has Line=%d: %+v", f.Line, f)
		}
	}
}

func TestDefaultPathAllowlistDoesNotSkipPublishedFiles(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	for _, path := range []string{"vendor/github.com/acme/y.go", "node_modules/x/a.js", "go.mod"} {
		got, err := scanner(t).Scan(leak.Input{
			Files: []leak.File{{Path: path, Content: []byte("TOKEN=" + tok + "\n")}},
		})
		if err != nil || len(got) == 0 {
			t.Errorf("%s: want a finding, got %+v %v", path, got, err)
		}
	}
}

// utf16Bytes encodes s as UTF-16 in the given byte order, with an optional BOM.
func utf16Bytes(s string, bigEndian, bom bool) []byte {
	var out []byte
	put := func(u uint16) {
		if bigEndian {
			out = append(out, byte(u>>8), byte(u))
		} else {
			out = append(out, byte(u), byte(u>>8))
		}
	}
	if bom {
		put(0xFEFF)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		put(u)
	}
	return out
}

func TestUTF16TextIsScanned(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	text := "A=1\r\nB=2\r\nTOKEN=" + tok + "\r\n"
	cases := []struct {
		name           string
		bigEndian, bom bool
	}{
		{"utf-16le with bom", false, true},
		{"utf-16le without bom", false, false},
		{"utf-16be with bom", true, true},
		{"utf-16be without bom", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := scanner(t).Scan(leak.Input{
				Files: []leak.File{{Path: ".env", Content: utf16Bytes(text, c.bigEndian, c.bom)}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 {
				t.Fatal("want a finding in UTF-16 text, got none")
			}
			for _, f := range got {
				if f.Path != ".env" || f.Line != 3 || f.Secret != leak.Redacted {
					t.Errorf("want a redacted finding on line 3 of .env, got %+v", f)
				}
			}
		})
	}
}

func TestUTF8BOMIsStripped(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	content := append([]byte{0xEF, 0xBB, 0xBF}, []byte("TOKEN="+tok+"\n")...)
	got, err := scanner(t).Scan(leak.Input{Files: []leak.File{{Path: "a.env", Content: content}}})
	if err != nil || len(got) != 1 || got[0].Line != 1 {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestRealBinaryIsStillSkipped(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}
	png = append(png, make([]byte, 64)...)
	png = append(png, []byte(testutil.FakeGitHubToken())...)
	got, err := scanner(t).Scan(leak.Input{Files: []leak.File{{Path: "x.png", Content: png}}})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestNULJustPastTheSniffWindowIsStillScanned(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	content := []byte(strings.Repeat("a", 8000))
	content = append(content, 0)
	content = append(content, []byte("\nTOKEN="+tok+"\n")...)
	got, err := scanner(t).Scan(leak.Input{Files: []leak.File{{Path: "big.txt", Content: content}}})
	if err != nil || len(got) != 1 || got[0].Line != 2 {
		t.Fatalf("want one finding on line 2, got %+v %v", got, err)
	}
}

// A UTF-16 BOM alone does not make a file UTF-16: Latin-1 or other 8-bit text
// can start with the same bytes. Without NULs after it, the file is scanned
// raw, as it was before UTF-16 support.
func TestBOMBytesWithoutUTF16ContentAreScannedRaw(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	for _, bom := range []string{"\xff\xfe", "\xfe\xff"} {
		got, err := scanner(t).Scan(leak.Input{Files: []leak.File{{Path: "latin1.env", Content: []byte(bom + "TOKEN=" + tok + "\n")}}})
		if err != nil || len(got) != 1 || got[0].Line != 1 {
			t.Errorf("BOM % x: want one finding on line 1, got %+v %v", bom, got, err)
		}
	}
}

// With a BOM, UTF-16 that is mostly non-Latin (so under the BOM-less 40%
// threshold) is still decoded: an ASCII secret in UTF-16 always brings NULs.
func TestUTF16WithBOMAndMostlyCJKIsScanned(t *testing.T) {
	text := strings.Repeat("中文字符", 100) + "\nTOKEN=" + testutil.FakeGitHubToken() + "\n"
	for _, be := range []bool{false, true} {
		got, err := scanner(t).Scan(leak.Input{Files: []leak.File{{Path: "notes.txt", Content: utf16Bytes(text, be, true)}}})
		if err != nil || len(got) != 1 || got[0].Line != 2 {
			t.Errorf("bigEndian=%v: want one finding on line 2, got %+v %v", be, got, err)
		}
	}
}
