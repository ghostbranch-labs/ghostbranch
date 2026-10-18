package cli

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
)

func TestCheckoutHint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		remote, free string
		inCheckout   bool
		want         string
	}{
		{"shadow", "ghostbranch-shadow", true, "git fetch shadow && git switch feat-x"},
		{"origin", "shadow", true, "git fetch origin && git switch feat-x"},
		{"", "shadow", true, "git remote add shadow https://github.com/acme/widget-shadow.git && git fetch shadow && git switch feat-x"},
		{"", "ghostbranch-shadow", true, "git remote add ghostbranch-shadow https://github.com/acme/widget-shadow.git && git fetch ghostbranch-shadow && git switch feat-x"},
		{"", "shadow", false, "git clone --origin shadow --branch feat-x https://github.com/acme/widget-shadow.git"},
	} {
		r := &engine.FeatureStartResult{}
		r.SetCheckout(checkoutArgv("feat-x", "acme/widget-shadow", tc.remote, tc.free, tc.inCheckout))
		if r.CheckoutHint != tc.want || len(r.Checkout) != strings.Count(tc.want, " && ")+1 {
			t.Errorf("%+v: %q %q", tc, r.CheckoutHint, r.Checkout)
		}
	}
}

func TestFreeRemoteName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		have []string
		want string
	}{
		{nil, "shadow"},
		{[]string{"origin"}, "shadow"},
		{[]string{"shadow"}, "ghostbranch-shadow"},
		{[]string{"shadow", "ghostbranch-shadow"}, "ghostbranch-shadow-2"},
		{[]string{"shadow", "ghostbranch-shadow", "ghostbranch-shadow-2"}, "ghostbranch-shadow-3"},
	} {
		m := map[string]string{}
		for _, n := range tc.have {
			m[n] = "x"
		}
		if got := freeRemoteName(m); got != tc.want {
			t.Errorf("%v: got %q, want %q", tc.have, got, tc.want)
		}
	}
}

// gitIn runs git in dir, failing the test on error.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestShadowRemoteDetection(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "remote", "add", "origin", "https://github.com/acme/widget.git")
	gitIn(t, dir, "remote", "add", "shadow", "git@github.com:Acme/Widget-Shadow.git")
	t.Chdir(dir)
	remote, free, in := shadowRemote("acme/widget-shadow")
	if remote != "shadow" || free != "ghostbranch-shadow" || !in {
		t.Errorf("got (%q, %q, %v)", remote, free, in)
	}
	if remote, free, in := shadowRemote("acme/other"); remote != "" || free != "ghostbranch-shadow" || !in {
		t.Errorf("no match: got (%q, %q, %v)", remote, free, in)
	}
}

func TestShadowRemoteSkipsUnsafeNames(t *testing.T) {
	for _, name := range []string{"a;touch x", "$(cmd)", "--upload-pack=cmd", "a&b"} {
		dir := t.TempDir()
		gitIn(t, dir, "init", "-q")
		gitIn(t, dir, "config", "remote."+name+".url", "https://github.com/acme/widget-shadow.git")
		t.Chdir(dir)
		remote, free, in := shadowRemote("acme/widget-shadow")
		if remote != "" || free != "shadow" || !in {
			t.Errorf("%q: got (%q, %q, %v)", name, remote, free, in)
		}
		r := &engine.FeatureStartResult{}
		r.SetCheckout(checkoutArgv("feat-x", "acme/widget-shadow", remote, free, in))
		if got := r.CheckoutHint; got != "git remote add shadow https://github.com/acme/widget-shadow.git && git fetch shadow && git switch feat-x" {
			t.Errorf("%q: hint %q", name, got)
		}
	}
}

func TestShadowRemoteOutsideACheckout(t *testing.T) {
	t.Chdir(t.TempDir())
	if remote, _, in := shadowRemote("acme/widget-shadow"); remote != "" || in {
		t.Errorf("got (%q, %v)", remote, in)
	}
}

func TestRedactURLs(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://user:ghp_x@github.com/o/r.git":            "https://github.com/o/r.git",
		"https://octo@github.com/o/r.git":                  "https://github.com/o/r.git",
		"ssh://git@github.com/o/r":                         "ssh://github.com/o/r",
		"fatal: repository 'https://t:x@h/o/r/' not found": "fatal: repository 'https://h/o/r/' not found",
		"git@github.com:o/r.git":                           "git@github.com:o/r.git",
	} {
		if got := redactURLs(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
