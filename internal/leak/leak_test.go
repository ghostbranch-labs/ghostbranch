package leak_test

import (
	"encoding/json"
	"strings"
	"testing"

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
