package engine

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
)

// chainRepo builds public main plus a rebased chain on it: one commit per
// files map. Sources are the rebased SHAs themselves (enough for these tests).
func chainRepo(t *testing.T, steps ...map[string]string) (*gitx.Git, string, []feature.GateCommit) {
	t.Helper()
	ctx := context.Background()
	g := testutil.BareRepo(t, t.TempDir())
	pm := testutil.Commit(t, g, "main", "public", map[string]string{"README.md": "# widget\n"})
	_ = g.UpdateRef(ctx, "refs/heads/feat", pm)
	var cs []feature.GateCommit
	for i, files := range steps {
		n := strconv.Itoa(i + 1)
		sha := testutil.Commit(t, g, "feat", "step "+n+"\n\nbody "+n, files)
		cs = append(cs, feature.GateCommit{Source: sha, Rebased: sha})
	}
	return g, pm, cs
}

func tip(t *testing.T, g *gitx.Git, cs []feature.GateCommit) (string, []gitx.TreeEntry) {
	t.Helper()
	T, es, err := publishedTree(context.Background(), g, cs[len(cs)-1].Rebased, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	return T, es
}

func TestDeriveChainPreserveKeepsEachCommit(t *testing.T) {
	t.Parallel()
	g, pm, cs := chainRepo(t, map[string]string{"a.txt": "a\n"}, map[string]string{"b.txt": "b\n"})
	T, es := tip(t, g, cs)
	links, dropped, err := deriveChain(context.Background(), g, config.Default(),
		chainInput{mode: config.ModePreserve, pm: pm, T: T, tEntries: es, commits: cs}, nil)
	if err != nil || len(links) != 2 || len(dropped) != 0 {
		t.Fatalf("links=%d dropped=%v err=%v", len(links), dropped, err)
	}
	if links[1].tree != T || !reflect.DeepEqual(links[0].sources, []string{cs[0].Source}) ||
		links[0].message != "step 1\n\nbody 1\n" || links[0].author.Email != "author@example.com" {
		t.Fatalf("links = %+v", links)
	}
}

// Review Focus 2.
func TestDeriveChainDropsCommitsEmptyAfterStrip(t *testing.T) {
	t.Parallel()
	g, pm, cs := chainRepo(t,
		map[string]string{"a.txt": "a\n"},
		map[string]string{".ghostbranch/notes.md": "private\n"},
		map[string]string{"b.txt": "b\n# BEGIN-INTERNAL\nx\n# END-INTERNAL\n"},
		map[string]string{"b.txt": "b\n# BEGIN-INTERNAL\ny\n# END-INTERNAL\n"})
	T, es := tip(t, g, cs)
	links, dropped, err := deriveChain(context.Background(), g, config.Default(),
		chainInput{mode: config.ModePreserve, pm: pm, T: T, tEntries: es, commits: cs}, nil)
	if err != nil || len(links) != 2 || !reflect.DeepEqual(dropped, []string{cs[1].Source, cs[3].Source}) {
		t.Fatalf("links=%d dropped=%v err=%v", len(links), dropped, err)
	}
}

func TestDeriveChainNothingToPublish(t *testing.T) {
	t.Parallel()
	g, pm, cs := chainRepo(t, map[string]string{".ghostbranch/notes.md": "private\n"})
	T, es := tip(t, g, cs)
	for _, mode := range []string{config.ModePreserve, config.ModeSquash} {
		_, _, err := deriveChain(context.Background(), g, config.Default(),
			chainInput{mode: mode, pm: pm, T: T, tEntries: es, commits: cs, message: "m"}, nil)
		if ce := contract.From(err); ce == nil || ce.Code != contract.NothingToPublish {
			t.Fatalf("%s: want NOTHING_TO_PUBLISH, got %v", mode, err)
		}
	}
}

func TestDeriveChainMarkerInvalidNamesTheCommit(t *testing.T) {
	t.Parallel()
	g, pm, cs := chainRepo(t,
		map[string]string{"a.txt": "# BEGIN-INTERNAL\nunclosed\n"},
		map[string]string{"a.txt": "fixed\n"})
	T, es := tip(t, g, cs)
	_, _, err := deriveChain(context.Background(), g, config.Default(),
		chainInput{mode: config.ModePreserve, pm: pm, T: T, tEntries: es, commits: cs}, nil)
	if ce := contract.From(err); ce == nil || ce.Code != contract.MarkerInvalid || ce.Details["commit"] != cs[0].Source {
		t.Fatalf("want MARKER_INVALID naming %s, got %v", cs[0].Source, err)
	}
}

func TestDeriveChainSquashIsOneLink(t *testing.T) {
	t.Parallel()
	g, pm, cs := chainRepo(t, map[string]string{"a.txt": "a\n"}, map[string]string{"b.txt": "b\n"})
	T, es := tip(t, g, cs)
	links, _, err := deriveChain(context.Background(), g, config.Default(),
		chainInput{mode: config.ModeSquash, pm: pm, T: T, tEntries: es, commits: cs, message: "Add a and b"}, nil)
	if err != nil || len(links) != 1 || links[0].tree != T || links[0].message != "Add a and b" ||
		!reflect.DeepEqual(links[0].sources, []string{cs[0].Source, cs[1].Source}) {
		t.Fatalf("squash = %+v %v", links, err)
	}
}

func TestPublicAuthor(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	orig := gitx.Identity{Name: "Jane", Email: "Jane.Doe@corp.example", When: when}
	pub := gitx.Identity{Name: "octo", Email: "42+octo@users.noreply.github.com", When: when.Add(time.Hour)}
	p := config.Default().Promote
	if got, ok := publicAuthor(p, orig, pub); !ok || got != (gitx.Identity{Name: "octo", Email: pub.Email, When: when}) {
		t.Fatalf("noreply = %+v", got)
	}
	p.AuthorEmail = config.AuthorKeep
	if got, _ := publicAuthor(p, orig, pub); got != orig {
		t.Fatalf("keep = %+v", got)
	}
	p.AuthorEmail = config.AuthorMap
	p.AuthorMap = map[string]string{"jane.doe@corp.example": "Jane Doe <7+jane@users.noreply.github.com>"}
	if got, ok := publicAuthor(p, orig, pub); !ok || got != (gitx.Identity{Name: "Jane Doe", Email: "7+jane@users.noreply.github.com", When: when}) {
		t.Fatalf("map = %+v", got)
	}
	if _, ok := publicAuthor(p, gitx.Identity{Email: "other@corp.example"}, pub); ok {
		t.Fatal("unmapped must report false")
	}
	links := []link{{author: orig}, {author: gitx.Identity{Email: "b@x.example"}}, {author: gitx.Identity{Email: "a@x.example"}}}
	_, err := chainAuthors(p, config.ModePreserve, links, pub)
	if ce := contract.From(err); ce == nil || ce.Code != contract.AuthorUnmapped || !reflect.DeepEqual(ce.Details["emails"], []string{"a@x.example", "b@x.example"}) {
		t.Fatalf("chainAuthors = %v", err)
	}
}

func newScanEngine() *Engine {
	return &Engine{NewScanner: func(r leak.Rules) (leak.Scanner, error) { return leak.NewGitleaks(r) }}
}

// Review Focus 1.
func TestScanChainCatchesASecretDeletedLater(t *testing.T) {
	t.Parallel()
	secret := "TOKEN=" + testutil.FakeGitHubToken() + "\n"
	g, pm, cs := chainRepo(t, map[string]string{"config.env": secret}, map[string]string{"config.env": testutil.Delete})
	ctx := context.Background()
	var steps []scanStep
	for _, c := range cs {
		_, es, err := publishedTree(ctx, g, c.Rebased, config.Default())
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps, scanStep{sources: []string{c.Source}, entries: es})
	}
	rep, _, err := newScanEngine().scanChain(ctx, g, config.Default(), pm, steps, nil)
	if err != nil || len(rep.Findings) != 1 || !reflect.DeepEqual(rep.Findings[0].Commits, []string{cs[0].Source}) {
		t.Fatalf("preserve scan = %+v %v", rep.Findings, err)
	}
	// Squash publishes only the tip: nothing to find.
	_, es := tip(t, g, cs)
	rep, _, err = newScanEngine().scanChain(ctx, g, config.Default(), pm, []scanStep{{entries: es}}, nil)
	if err != nil || len(rep.Findings) != 0 {
		t.Fatalf("squash scan = %+v %v", rep.Findings, err)
	}
}

func TestScanChainMergesARepeatIntoOneFinding(t *testing.T) {
	t.Parallel()
	g, pm, cs := chainRepo(t, map[string]string{"a.txt": "a\n"}, map[string]string{"b.txt": "b\n"})
	ctx := context.Background()
	leaky := "token " + testutil.FakeGitHubToken()
	var steps []scanStep
	for _, c := range cs {
		_, es, err := publishedTree(ctx, g, c.Rebased, config.Default())
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps, scanStep{sources: []string{c.Source}, entries: es,
			texts: []leak.Text{{Where: "commit_message", Content: leaky}}})
	}
	rep, _, err := newScanEngine().scanChain(ctx, g, config.Default(), pm, steps, nil)
	if err != nil || len(rep.Findings) != 1 || !reflect.DeepEqual(rep.Findings[0].Commits, []string{cs[0].Source, cs[1].Source}) {
		t.Fatalf("findings = %+v %v", rep.Findings, err)
	}
}

func TestMergeWarnings(t *testing.T) {
	t.Parallel()
	wf := func(paths ...string) contract.Warning {
		return contract.Warning{Code: contract.WorkflowChanged, Message: "workflow files are new or changed: " + strings.Join(paths, ", "),
			Details: map[string]any{"paths": paths}}
	}
	large := contract.Warning{Code: contract.LargeFile, Message: "big: z", Details: map[string]any{"paths": []string{"z"}}}
	many := []string{"a", "b", "c", "d", "e", "f", "g"}
	cases := []struct {
		name      string
		dst, more []contract.Warning
		want      []contract.Warning
	}{
		{"two paths become one warning", []contract.Warning{wf("a.yml")}, []contract.Warning{wf("b.yml")},
			[]contract.Warning{wf("a.yml", "b.yml")}},
		{"a repeated path is listed once", []contract.Warning{wf("a.yml")}, []contract.Warning{wf("a.yml", "b.yml")},
			[]contract.Warning{wf("a.yml", "b.yml")}},
		{"more than five paths are cut", []contract.Warning{wf(many[:4]...)}, []contract.Warning{wf(many[4:]...)},
			[]contract.Warning{{Code: contract.WorkflowChanged, Message: "workflow files are new or changed: a, b, c, d, e, and 2 more",
				Details: map[string]any{"paths": many}}}},
		{"another code is appended", []contract.Warning{wf("a.yml")}, []contract.Warning{large},
			[]contract.Warning{wf("a.yml"), large}},
		{"an exact repeat is skipped", []contract.Warning{large}, []contract.Warning{large}, []contract.Warning{large}},
		{"into nothing", nil, []contract.Warning{wf("a.yml")}, []contract.Warning{wf("a.yml")}},
	}
	for _, tc := range cases {
		if got := mergeWarnings(tc.dst, tc.more); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
}

func TestAssertChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g, pm, cs := chainRepo(t, map[string]string{"a.txt": "a\n"}, map[string]string{"b.txt": "b\n"})
	T, _ := tip(t, g, cs)
	who := gitx.Identity{Name: "octo", Email: "42+octo@users.noreply.github.com", When: testutil.Base}
	write := func(parent, tree string) string {
		sha, err := g.CommitTree(ctx, tree, []string{parent}, "m", who, who)
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}
	t1, _, err := publishedTree(ctx, g, cs[0].Rebased, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	p1 := write(pm, t1)
	p2 := write(p1, T)
	C := p2 // no overlay or guard rewrites in this repo: C's tree is T
	good := []plan.Commit{{Commit: p1, Tree: t1, Parent: pm}, {Commit: p2, Tree: T, Parent: p1}}
	set := overlay.NewSet(".ghostbranch/**")
	names := workflow.Names{Public: "acme/widget", Shadow: "acme/widget-shadow"}
	if err := assertChain(ctx, g, good, T, C, pm, set, names); err != nil {
		t.Fatalf("good chain: %v", err)
	}
	ovTree, err := g.MakeTree(ctx, []gitx.TreeEntry{{Mode: "100644", Type: "blob", SHA: mustHash(t, g, "x\n"), Path: ".ghostbranch/x.md"}})
	if err != nil {
		t.Fatal(err)
	}
	pOv := write(pm, ovTree)
	bad := map[string][]plan.Commit{
		"no commits":           {},
		"wrong parent":         {{Commit: p2, Tree: T, Parent: pm}},
		"mid tree not planned": {{Commit: p1, Tree: T, Parent: pm}, {Commit: p2, Tree: T, Parent: p1}},
		"tip is not T":         {{Commit: p1, Tree: t1, Parent: pm}},
		"overlay path":         {{Commit: pOv, Tree: ovTree, Parent: pm}, {Commit: write(pOv, T), Tree: T, Parent: pOv}},
	}
	merge, err := g.CommitTree(ctx, T, []string{p1, pm}, "merge", who, who)
	if err != nil {
		t.Fatal(err)
	}
	bad["merge commit"] = []plan.Commit{{Commit: p1, Tree: t1, Parent: pm}, {Commit: merge, Tree: T, Parent: p1}}
	for name, chain := range bad {
		if ce := contract.From(assertChain(ctx, g, chain, T, C, pm, set, names)); ce == nil || ce.Code != contract.Internal {
			t.Fatalf("%s: want INTERNAL, got %v", name, ce)
		}
	}
}

func mustHash(t *testing.T, g *gitx.Git, s string) string {
	t.Helper()
	sha, err := g.HashObject(context.Background(), []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return sha
}
