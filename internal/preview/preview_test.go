package preview_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
)

func TestOpsSerializeWithNulls(t *testing.T) {
	p := preview.New(nil)
	p.Add(preview.PushRef("acme/x", "refs/heads/main", "", "abc"), preview.Grant("acme/x", "acme/x-shadow", nil))
	b, _ := json.Marshal(p)
	want := `{"fingerprint":{},"ops":[{"from":null,"op":"push_ref","ref":"refs/heads/main","repo":"acme/x","to":"abc"},` +
		`{"expires_at":null,"op":"grant","public":"acme/x","shadow":"acme/x-shadow"}]}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	exp := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	b, _ = json.Marshal(preview.Grant("a/b", "a/c", &exp))
	if !strings.Contains(string(b), `"expires_at":"2026-10-06T18:00:00Z"`) {
		t.Fatal(string(b))
	}
}

func TestDescribe(t *testing.T) {
	p := preview.New(nil)
	p.Add(preview.CreateRepo("acme/x-shadow"), preview.PushRef("acme/x-shadow", "refs/heads/main", "", "0123456789abcdef"),
		preview.PushRef("acme/x-shadow", "refs/heads/main", "0123456789abcdef", "fedcba9876543210"),
		preview.OpenPR("acme/x-shadow", "g", "main", true), preview.RunGate("feat-x"), preview.MoveFile("a", "b"),
		preview.PushRef("acme/x-shadow", "refs/ghostbranch/features/f", "0123456789abcdef", ""),
		preview.OverlayFile("acme/x-shadow", ".github/workflows/ghostbranch-sync.yml", "add"))
	got := strings.Join(p.Describe(), "\n")
	for _, want := range []string{"create private repo acme/x-shadow", "create refs/heads/main in acme/x-shadow at 0123456789ab",
		"move refs/heads/main in acme/x-shadow from 0123456789ab to fedcba987654", "open a draft PR in acme/x-shadow from g into main",
		"run the gate for feat-x", "move a to b", "update refs/ghostbranch/features/f in acme/x-shadow from 0123456789ab",
		"add .github/workflows/ghostbranch-sync.yml on the ghostbranch branch of acme/x-shadow"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
