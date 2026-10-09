package workspace_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
func home() (string, error)                       { return "/home/u", nil }

func TestHomeDir(t *testing.T) {
	cases := []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"windows", map[string]string{"GHOSTBRANCH_HOME": "/x"}, "/x"},
		{"windows", map[string]string{"LOCALAPPDATA": `C:\Users\u\AppData\Local`}, filepath.Join(`C:\Users\u\AppData\Local`, "ghostbranch")},
		{"darwin", nil, filepath.Join("/home/u", "Library", "Application Support", "ghostbranch")},
		{"linux", map[string]string{"XDG_STATE_HOME": "/state"}, filepath.Join("/state", "ghostbranch")},
		{"linux", nil, filepath.Join("/home/u", ".local", "state", "ghostbranch")},
	}
	for _, c := range cases {
		got, err := workspace.HomeDir(env(c.env), c.goos, home)
		if err != nil || got != c.want {
			t.Errorf("%s %v: got %q %v, want %q", c.goos, c.env, got, err, c.want)
		}
	}
	if _, err := workspace.HomeDir(env(nil), "windows", home); err == nil {
		t.Error("windows without LOCALAPPDATA must fail")
	}
}

func TestOpenSaveAddFind(t *testing.T) {
	root := t.TempDir()
	ws, err := workspace.Open(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	p := workspace.Pair{Public: "acme/widget", Shadow: "acme/widget-shadow"}
	if !ws.AddPair(p) || ws.AddPair(p) {
		t.Fatal("AddPair must add once")
	}
	if err := ws.Save(); err != nil {
		t.Fatal(err)
	}
	again, _ := workspace.Open(root, "default")
	if got, ok := again.Find("acme/widget-shadow"); !ok || got != p {
		t.Fatalf("Find by shadow = %+v %v", got, ok)
	}
	if got, ok := again.Find("ACME/Widget"); !ok || got != p {
		t.Fatalf("Find is case-insensitive: %+v %v", got, ok)
	}
	if again.MirrorDir(p) != filepath.Join(root, "workspaces", "default", "pairs", "acme__widget", "mirror.git") {
		t.Fatalf("MirrorDir = %s", again.MirrorDir(p))
	}
	if _, err := workspace.Open(root, "../evil"); contract.From(err).Code != contract.BadFlag {
		t.Fatalf("bad workspace name: %v", err)
	}
}

func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p", "lock")
	now := time.Now()
	unlock, err := workspace.Lock(path, now, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Lock(path, now, "test"); contract.From(err).Code != contract.Locked {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	unlock2, err := workspace.Lock(path, now, "test")
	if err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	unlock2()
}

func TestParseRemoteURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/widget.git": "acme/widget",
		"https://github.com/acme/widget":     "acme/widget",
		"git@github.com:acme/widget.git":     "acme/widget",
		"ssh://git@github.com/acme/widget":   "acme/widget",
		"https://gitlab.com/acme/widget.git": "",
		"/local/path.git":                    "",
	}
	for u, want := range cases {
		got, ok := workspace.ParseRemoteURL(u)
		if got != want || ok != (want != "") {
			t.Errorf("ParseRemoteURL(%q) = %q %v", u, got, ok)
		}
	}
}

func TestResolve(t *testing.T) {
	ws, _ := workspace.Open(t.TempDir(), "default")
	_, err := ws.Resolve("", nil)
	if contract.From(err).Code != contract.PairAmbiguous {
		t.Fatalf("empty workspace: %v", err)
	}
	a := workspace.Pair{Public: "acme/a", Shadow: "acme/a-shadow"}
	b := workspace.Pair{Public: "acme/b", Shadow: "acme/b-shadow"}
	ws.AddPair(a)
	if got, err := ws.Resolve("", nil); err != nil || got != a {
		t.Fatalf("single pair: %+v %v", got, err)
	}
	ws.AddPair(b)
	if _, err := ws.Resolve("", nil); contract.From(err).Code != contract.PairAmbiguous {
		t.Fatalf("two pairs, no hint: %v", err)
	}
	if got, _ := ws.Resolve("", []workspace.Remote{{Name: "origin", URL: "git@github.com:acme/b-shadow.git"}}); got != b {
		t.Fatalf("by cwd remote: %+v", got)
	}
	if got, _ := ws.Resolve("acme/a", []workspace.Remote{{Name: "origin", URL: "git@github.com:acme/b.git"}}); got != a {
		t.Fatalf("--repo wins: %+v", got)
	}
	var ce *contract.Error
	if _, err := ws.Resolve("acme/zzz", nil); !errors.As(err, &ce) || ce.Field != "repo" {
		t.Fatalf("unknown --repo: %v", err)
	}
}

func TestResolveDotMeansThisCheckout(t *testing.T) {
	t.Parallel()
	ws, _ := workspace.Open(t.TempDir(), "default")
	a := workspace.Pair{Public: "acme/a", Shadow: "acme/a-shadow"}
	b := workspace.Pair{Public: "acme/b", Shadow: "acme/b-shadow"}
	ws.AddPair(a)
	ws.AddPair(b)
	if got, err := ws.Resolve(".", []workspace.Remote{{Name: "origin", URL: "git@github.com:acme/b.git"}}); err != nil || got != b {
		t.Fatalf("--repo . by remote: %+v %v", got, err)
	}

	// One pair, but the checkout's remotes name another repo: say so rather
	// than falling back to the workspace's only pair.
	one, _ := workspace.Open(t.TempDir(), "default")
	one.AddPair(a)
	_, err := one.Resolve(".", []workspace.Remote{{Name: "origin", URL: "https://github.com/acme/widget.git"}})
	var ce *contract.Error
	if !errors.As(err, &ce) || ce.Code != contract.PairAmbiguous || ce.Field != "repo" ||
		!strings.Contains(ce.Message, `the remotes of this checkout name no repo in workspace "default"`) ||
		ce.Details["public"] != "acme/widget" {
		t.Fatalf("--repo . with other remotes: %+v", err)
	}
}

func TestResolveNotRegisteredNamesThePublicRepo(t *testing.T) {
	t.Parallel()
	ws, _ := workspace.Open(t.TempDir(), "default")
	var ce *contract.Error
	if _, err := ws.Resolve("acme/x", nil); !errors.As(err, &ce) || ce.Details["public"] != "acme/x" {
		t.Fatalf("unknown --repo: %+v", err)
	}
	// Empty workspace inside a checkout: the remote that parses names the repo.
	_, err := ws.Resolve("", []workspace.Remote{{Name: "upstream", URL: "https://example.com/nope.git"}, {Name: "origin", URL: "git@github.com:acme/widget.git"}})
	if !errors.As(err, &ce) || ce.Code != contract.PairAmbiguous || ce.Details["public"] != "acme/widget" {
		t.Fatalf("empty workspace: %+v", err)
	}
	// Several pairs stay a candidates error.
	ws.AddPair(workspace.Pair{Public: "acme/a", Shadow: "acme/a-shadow"})
	ws.AddPair(workspace.Pair{Public: "acme/b", Shadow: "acme/b-shadow"})
	if _, err := ws.Resolve("", nil); !errors.As(err, &ce) || ce.Details["candidates"] == nil || ce.Details["public"] != nil {
		t.Fatalf("several: %+v", err)
	}
}

func TestResolveNeverGuessesTheShadowRepoAsPublic(t *testing.T) {
	t.Parallel()
	ws, _ := workspace.Open(t.TempDir(), "default")
	var ce *contract.Error
	both := []workspace.Remote{
		{Name: "ghostbranch-shadow", URL: "git@github.com:acme/widget-shadow.git"},
		{Name: "origin", URL: "git@github.com:acme/widget.git"},
	}
	if _, err := ws.Resolve("", both); !errors.As(err, &ce) || ce.Details["public"] != "acme/widget" {
		t.Fatalf("shadow remote listed first: %+v", err)
	}
	onlyShadow := []workspace.Remote{{Name: "shadow", URL: "git@github.com:acme/widget-shadow.git"}}
	if _, err := ws.Resolve("", onlyShadow); !errors.As(err, &ce) || ce.Details["public"] != nil {
		t.Fatalf("only a shadow remote: %+v", err)
	}
	if _, err := ws.Resolve("acme/widget-shadow", nil); !errors.As(err, &ce) || ce.Details["public"] != nil {
		t.Fatalf("--repo naming a shadow: %+v", err)
	}
	shadowOrigin := []workspace.Remote{{Name: "origin", URL: "https://github.com/acme/widget-shadow"}}
	if _, err := ws.Resolve("", shadowOrigin); !errors.As(err, &ce) || ce.Details["public"] != nil {
		t.Fatalf("origin is the shadow repo: %+v", err)
	}
	two := []workspace.Remote{{Name: "a", URL: "git@github.com:acme/one.git"}, {Name: "b", URL: "git@github.com:acme/two.git"}}
	if _, err := ws.Resolve("", two); !errors.As(err, &ce) || ce.Details["public"] != nil {
		t.Fatalf("two different repos, no origin: %+v", err)
	}
	if _, err := ws.Resolve(".", nil); !errors.As(err, &ce) || ce.Field != "repo" ||
		!strings.Contains(ce.Message, "this directory is not a checkout with a GitHub remote") {
		t.Fatalf("--repo . outside a checkout: %+v", err)
	}
}

func TestRemovePair(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ws, err := workspace.Open(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	a := workspace.Pair{Public: "acme/widget", Shadow: "acme/widget-shadow"}
	b := workspace.Pair{Public: "acme/other", Shadow: "acme/other-shadow"}
	ws.AddPair(a)
	ws.AddPair(b)
	if ws.RemovePair("acme/nope") {
		t.Fatal("removing an unregistered pair must report false")
	}
	if !ws.RemovePair("ACME/Widget") {
		t.Fatal("RemovePair is case-insensitive on the public repo")
	}
	if _, ok := ws.Find("acme/widget"); ok || len(ws.Pairs) != 1 || ws.Pairs[0] != b {
		t.Fatalf("pairs after remove: %+v", ws.Pairs)
	}
	if ws.RemovePair("acme/widget") {
		t.Fatal("a second remove must report false")
	}
	if err := ws.Save(); err != nil {
		t.Fatal(err)
	}
	again, _ := workspace.Open(root, "default")
	if len(again.Pairs) != 1 || again.Pairs[0] != b {
		t.Fatalf("saved pairs: %+v", again.Pairs)
	}
}
