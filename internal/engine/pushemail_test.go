package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

const gh007Reason = "push declined due to email privacy restrictions: error: GH007: Your push would publish a private email address."

// gh007Repo is a mirror with a public main commit and a feature commit on
// top of it, both made by email.
func gh007Repo(t *testing.T, email string) (g *gitx.Git, pub, feat string) {
	t.Helper()
	ctx := context.Background()
	g = testutil.BareRepo(t, t.TempDir())
	tree, err := g.MakeTree(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := gitx.Identity{Name: "t", Email: email, When: time.Unix(1700000000, 0)}
	pub, err = g.CommitTree(ctx, tree, nil, "Initial commit", id, id)
	if err != nil {
		t.Fatal(err)
	}
	feat, err = g.CommitTree(ctx, tree, []string{pub}, "add the thing\n\nbody", id, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.UpdateRef(ctx, "refs/remotes/public/main", pub); err != nil {
		t.Fatal(err)
	}
	return g, pub, feat
}

func TestExplainGH007TipOnPublicMain(t *testing.T) {
	t.Parallel()
	g, pub, _ := gh007Repo(t, "me@example.com")
	refused := &gitx.RefusedError{Refs: []string{"refs/heads/probe2"}, Reason: gh007Reason}
	msg, det, ok := explainGH007(context.Background(), g, []string{pub + ":refs/heads/probe2"}, refused, "acme/widget")
	if !ok {
		t.Fatal("no explanation")
	}
	want := `GitHub refused the push because the newest commit on probe2 (` + pub[:7] + `, "Initial commit")
has your private email, me@example.com, and your account blocks pushes that expose it.
That commit is already public in acme/widget; your own commits are fine.
Fix one of these:
  - untick "Block command line pushes that expose my email" at https://github.com/settings/emails, or
  - add any new commit to acme/widget's main using your no-reply address, then run this again`
	if msg != want {
		t.Fatalf("message:\n%s\nwant:\n%s", msg, want)
	}
	if det["cause"] != "email_private" || det["commit"] != pub || det["email"] != "me@example.com" ||
		det["branch"] != "probe2" || det["settings_url"] != "https://github.com/settings/emails" {
		t.Fatalf("details = %v", det)
	}
}

func TestExplainGH007OwnCommit(t *testing.T) {
	t.Parallel()
	g, _, feat := gh007Repo(t, "me@example.com")
	refused := &gitx.RefusedError{Refs: []string{"refs/heads/ghostbranch-gate/probe2"}, Reason: gh007Reason}
	msg, det, ok := explainGH007(context.Background(), g, []string{"+" + feat + ":refs/heads/ghostbranch-gate/probe2"}, refused, "acme/widget")
	if !ok {
		t.Fatal("no explanation")
	}
	want := `GitHub refused the push because the newest commit on ghostbranch-gate/probe2 (` + feat[:7] + `, "add the thing")
has your private email, me@example.com, and your account blocks pushes that expose it.
Fix one of these:
  - untick "Block command line pushes that expose my email" at https://github.com/settings/emails, or
  - set git's user.email to your GitHub no-reply address (shown on that page), recreate the commit, and push again`
	if msg != want {
		t.Fatalf("message:\n%s\nwant:\n%s", msg, want)
	}
	if det["commit"] != feat || det["branch"] != "ghostbranch-gate/probe2" {
		t.Fatalf("details = %v", det)
	}
}

// Only the first GH007 ref is explained.
func TestExplainGH007FirstGH007Ref(t *testing.T) {
	t.Parallel()
	g, pub, feat := gh007Repo(t, "me@example.com")
	refused := &gitx.RefusedError{
		Refs:       []string{"refs/heads/a", "refs/heads/b", "refs/heads/c"},
		RefReasons: []string{"pre-receive hook declined", gh007Reason, gh007Reason},
		Reason:     "x",
	}
	msg, _, ok := explainGH007(context.Background(), g,
		[]string{pub + ":refs/heads/a", feat + ":refs/heads/b", pub + ":refs/heads/c"}, refused, "acme/widget")
	if !ok || !strings.Contains(msg, "newest commit on b ("+feat[:7]) {
		t.Fatalf("ok=%v msg=%s", ok, msg)
	}
}

// When the lookup fails the caller keeps its own message.
func TestExplainGH007LookupFailsLeavesMessage(t *testing.T) {
	t.Parallel()
	g, _, _ := gh007Repo(t, "me@example.com")
	ctx := context.Background()
	missing := strings.Repeat("a", 40)
	for name, tc := range map[string]struct {
		specs   []string
		refused *gitx.RefusedError
	}{
		"missing commit": {[]string{missing + ":refs/heads/x"}, &gitx.RefusedError{Refs: []string{"refs/heads/x"}, Reason: gh007Reason}},
		"no refspec":     {nil, &gitx.RefusedError{Refs: []string{"refs/heads/x"}, Reason: gh007Reason}},
		"not GH007":      {[]string{missing + ":refs/heads/x"}, &gitx.RefusedError{Refs: []string{"refs/heads/x"}, Reason: "pre-receive hook declined"}},
	} {
		if msg, det, ok := explainGH007(ctx, g, tc.specs, tc.refused, "acme/widget"); ok || msg != "" || det != nil {
			t.Errorf("%s: got %q %v %v", name, msg, det, ok)
		}
	}
}
