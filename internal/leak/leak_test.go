package leak_test

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func scanner(t *testing.T, patterns ...string) *leak.Gitleaks {
	t.Helper()
	s, err := leak.NewGitleaks(leak.Rules{Overlay: overlay.NewSet("internal/**"), BlockPatterns: patterns, LargeFile: 4})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCleanInputHasNoFindings(t *testing.T) {
	rep, err := scanner(t).Scan(leak.Input{
		Paths: []string{"README.md"},
		Files: []leak.File{{Path: "README.md", Content: []byte("# hello\n")}},
		Texts: []leak.Text{{Where: "commit_message", Content: "Add a greeting"}},
	})
	got := rep.Findings
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestSecretsAreFoundAndRedacted(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	rep, err := scanner(t).Scan(leak.Input{
		Paths: []string{"config.env"},
		Files: []leak.File{{Path: "config.env", Content: []byte("A=1\nTOKEN=" + tok + "\n")}},
		Texts: []leak.Text{{Where: "pr_body", Content: "temporary token " + tok}},
	})
	got := rep.Findings
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
	rep, err := scanner(t).Scan(leak.Input{Paths: []string{"src/a.go", ".ghostbranch/notes.md", "internal/plan.md"}})
	got := rep.Findings
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range got {
		if f.Rule == "overlay-path" {
			paths = append(paths, f.Path)
		}
	}
	if want := []string{".ghostbranch/notes.md", "internal/plan.md"}; len(got) != 2 || !reflect.DeepEqual(paths, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestLinesAreOneBased(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	rep, err := scanner(t).Scan(leak.Input{
		Files: []leak.File{{Path: "a.env", Content: []byte("TOKEN=" + tok + "\n")}},
		Texts: []leak.Text{{Where: "pr_body", Content: "TOKEN=" + tok}},
	})
	got := rep.Findings
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
		rep, err := scanner(t).Scan(leak.Input{
			Files: []leak.File{{Path: path, Content: []byte("TOKEN=" + tok + "\n")}},
		})
		got := rep.Findings
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
			rep, err := scanner(t).Scan(leak.Input{
				Files: []leak.File{{Path: ".env", Content: utf16Bytes(text, c.bigEndian, c.bom)}},
			})
			got := rep.Findings
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
	rep, err := scanner(t).Scan(leak.Input{Files: []leak.File{{Path: "a.env", Content: content}}})
	got := rep.Findings
	if err != nil || len(got) != 1 || got[0].Line != 1 {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestNULJustPastTheSniffWindowIsStillScanned(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	content := []byte(strings.Repeat("a", 8000))
	content = append(content, 0)
	content = append(content, []byte("\nTOKEN="+tok+"\n")...)
	rep, err := scanner(t).Scan(leak.Input{Files: []leak.File{{Path: "big.txt", Content: content}}})
	got := rep.Findings
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
		rep, err := scanner(t).Scan(leak.Input{Files: []leak.File{{Path: "latin1.env", Content: []byte(bom + "TOKEN=" + tok + "\n")}}})
		got := rep.Findings
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
		rep, err := scanner(t).Scan(leak.Input{Files: []leak.File{{Path: "notes.txt", Content: utf16Bytes(text, be, true)}}})
		got := rep.Findings
		if err != nil || len(got) != 1 || got[0].Line != 2 {
			t.Errorf("bigEndian=%v: want one finding on line 2, got %+v %v", be, got, err)
		}
	}
}

func TestVersionNamesGitleaks(t *testing.T) {
	if !strings.HasPrefix(scanner(t).Version(), "gitleaks") {
		t.Fatalf("Version = %q", scanner(t).Version())
	}
}

var idPattern = regexp.MustCompile(`^f-[0-9a-f]{12}$`)

func TestSecretsInsideBinariesAreFound(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	png := append([]byte{0x89, 'P', 'N', 'G', 0, 0, 0, 13}, []byte("tEXt\x00"+tok+"\x00")...)
	rep, err := scanner(t).Scan(leak.Input{Paths: []string{"logo.png"}, Files: []leak.File{{Path: "logo.png", Content: png}}})
	if err != nil || len(rep.Findings) != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	f := rep.Findings[0]
	if f.Path != "logo.png" || f.Line != 0 || !f.Waivable || f.Secret != leak.Redacted || !idPattern.MatchString(f.ID) {
		t.Fatalf("finding = %+v", f)
	}
}

func TestMostlyCJKUTF16WithoutBOMIsScanned(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	var b []byte
	for _, u := range utf16.Encode([]rune(strings.Repeat("漢字", 400) + " token=" + tok + "\n")) {
		b = append(b, byte(u), byte(u>>8))
	}
	rep, err := scanner(t).Scan(leak.Input{Paths: []string{"notes.txt"}, Files: []leak.File{{Path: "notes.txt", Content: b}}})
	if err != nil || len(rep.Findings) == 0 {
		t.Fatalf("the ASCII token inside mostly-CJK UTF-16 was missed: %+v %v", rep, err)
	}
}

func TestBlockPatternsInFilesPathsAndTexts(t *testing.T) {
	rep, err := scanner(t, "project-falcon", "JIRA-[0-9]+").Scan(leak.Input{
		Paths: []string{"src/project-falcon/x.go"},
		Files: []leak.File{{Path: "src/project-falcon/x.go", Content: []byte("package x\n// see JIRA-123\n")}},
		Texts: []leak.Text{{Where: "pr_body", Content: "fixes JIRA-9"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		where, match string
		line         int
		waivable     bool
	}
	got := map[key]bool{}
	for _, f := range rep.Findings {
		if f.Rule != leak.BlockRule || f.Secret != "" {
			t.Errorf("block-pattern finding shape: %+v", f)
		}
		got[key{f.Where, f.Match, f.Line, f.Waivable}] = true
	}
	want := map[key]bool{{"path", "project-falcon", 0, true}: true, {"tree", "JIRA-123", 2, true}: true, {"pr_body", "JIRA-9", 1, false}: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %+v", rep.Findings)
	}
}

func TestFindingIDs(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	scan := func(content string) leak.Finding {
		t.Helper()
		rep, err := scanner(t).Scan(leak.Input{Paths: []string{"a.env"}, Files: []leak.File{{Path: "a.env", Content: []byte(content)}}})
		if err != nil || len(rep.Findings) != 1 {
			t.Fatalf("%+v %v", rep, err)
		}
		return rep.Findings[0]
	}
	a, moved := scan("T="+tok+"\n"), scan("# a comment\n\nT="+tok+"\n")
	if a.ID != moved.ID || a.Line == moved.Line || !idPattern.MatchString(a.ID) {
		t.Fatalf("an id must survive a line move: %+v vs %+v", a, moved)
	}
	other := scan("T=" + tok[:len(tok)-1] + "X\n")
	if other.ID == a.ID {
		t.Fatal("a changed secret must get a new id")
	}
	if a.ID != leak.FindingID(a.Rule, "tree", "a.env", a.MatchSHA256) {
		t.Fatal("FindingID disagrees with the scan")
	}
}

// Review Focus 4.
func TestDuplicateSecrets(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	body := []byte("A=" + tok + "\nB=" + tok + "\n")
	rep, err := scanner(t).Scan(leak.Input{Paths: []string{"one.env", "two.env"},
		Files: []leak.File{{Path: "one.env", Content: body}, {Path: "two.env", Content: body}}})
	if err != nil || len(rep.Findings) != 2 || rep.Findings[0].ID == rep.Findings[1].ID {
		t.Fatalf("want one finding per file with different ids: %+v %v", rep.Findings, err)
	}
	if rep.Findings[0].Line != 1 {
		t.Fatalf("the kept finding is the first line: %+v", rep.Findings[0])
	}
}

func TestWarnings(t *testing.T) {
	rep, err := scanner(t).Scan(leak.Input{
		Paths: []string{".github/workflows/new.yml", ".github/workflows/same.yml", "big.bin", "small.txt"},
		Files: []leak.File{
			{Path: ".github/workflows/new.yml", Content: []byte("on: push\n"), Changed: true},
			{Path: ".github/workflows/same.yml", Content: []byte("on: push\n")},
			{Path: "big.bin", Content: []byte{0x89, 'P', 'N', 'G', 0, 0, 1, 2}, Changed: true}, // NULs on both sides: binary, not UTF-16
			{Path: "small.txt", Content: []byte("ok"), Changed: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[contract.WarningCode]any{}
	for _, w := range rep.Warnings {
		got[w.Code] = w.Details["paths"]
	}
	want := map[contract.WarningCode]any{
		contract.WorkflowChanged: []string{".github/workflows/new.yml"},
		contract.LargeFile:       []string{".github/workflows/new.yml", "big.bin"},
		contract.BinaryFile:      []string{"big.bin"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("warnings = %+v", rep.Warnings)
	}
}

func TestNoSecretOrHintInJSON(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	rep, _ := scanner(t).Scan(leak.Input{Paths: []string{"a.env"}, Files: []leak.File{{Path: "a.env", Content: []byte("T=" + tok + "\n")}}})
	if len(rep.Findings) != 1 || rep.Findings[0].Hint != tok[:4]+"…"+tok[len(tok)-4:] {
		t.Fatalf("hint = %+v", rep.Findings)
	}
	b, _ := json.Marshal(rep.Findings)
	if strings.Contains(string(b), tok[4:12]) || strings.Contains(string(b), "…") || strings.Contains(string(b), "match_sha256") {
		t.Fatalf("serialized findings leak the secret or hint: %s", b)
	}
}

func TestEmptyBlockPatternMatchesAreIgnored(t *testing.T) {
	rep, err := scanner(t, "x*").Scan(leak.Input{Paths: []string{"a.dat"}, Files: []leak.File{{Path: "a.dat", Content: []byte("abc\n")}}})
	if err != nil || len(rep.Findings) != 0 {
		t.Fatalf("empty matches must not block: %+v %v", rep.Findings, err)
	}
	rep, err = scanner(t, "x*").Scan(leak.Input{Paths: []string{"a.dat"}, Files: []leak.File{{Path: "a.dat", Content: []byte("xx\n")}}})
	if err != nil || len(rep.Findings) != 1 || rep.Findings[0].Match != "xx" {
		t.Fatalf("want one finding for xx: %+v %v", rep.Findings, err)
	}
}

func TestFindingCommitsAreOmittedWhenEmpty(t *testing.T) {
	b, _ := json.Marshal(leak.Finding{ID: "f-000000000000", Rule: "r", Where: "tree"})
	if strings.Contains(string(b), "commits") {
		t.Fatalf("empty commits must be omitted: %s", b)
	}
	b, _ = json.Marshal(leak.Finding{ID: "f-000000000000", Rule: "r", Where: "tree", Commits: []string{"abc"}})
	if !strings.Contains(string(b), `"commits":["abc"]`) {
		t.Fatalf("commits missing: %s", b)
	}
}

// Probabilistic: the race it guards against (gitleaks' global viper) only
// shows up when constructions overlap.
func TestNewGitleaksIsSafeConcurrently(t *testing.T) {
	tok := testutil.FakeGitHubToken()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := leak.NewGitleaks(leak.Rules{Overlay: overlay.NewSet("internal/**"), LargeFile: 4})
			if err != nil {
				t.Error(err)
				return
			}
			rep, err := s.Scan(leak.Input{Files: []leak.File{{Path: "a.env", Content: []byte("TOKEN=" + tok + "\n")}}})
			if err != nil || len(rep.Findings) == 0 {
				t.Errorf("token not found: %+v %v", rep.Findings, err)
			}
		}()
	}
	wg.Wait()
}
