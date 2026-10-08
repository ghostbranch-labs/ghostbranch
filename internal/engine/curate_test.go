package engine_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func writeCurate(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "curate.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Review Focus 3 (shape).
func TestLoadCurateShape(t *testing.T) {
	t.Parallel()
	good := `{"commits": [{"from": ["abcdef1"], "message": "feat: a"}]}`
	if gs, err := engine.LoadCurate(writeCurate(t, good)); err != nil || len(gs) != 1 || gs[0].From[0] != "abcdef1" {
		t.Fatalf("good = %+v %v", gs, err)
	}
	for name, body := range map[string]string{
		"not json":      `{`,
		"unknown key":   `{"commits": [], "extra": 1}`,
		"no groups":     `{"commits": []}`,
		"empty from":    `{"commits": [{"from": [], "message": "m"}]}`,
		"blank message": `{"commits": [{"from": ["abcdef1"], "message": "  "}]}`,
		"short sha":     `{"commits": [{"from": ["abc"], "message": "m"}]}`,
		"not hex":       `{"commits": [{"from": ["zzzzzzz"], "message": "m"}]}`,
	} {
		_, err := engine.LoadCurate(writeCurate(t, body))
		if ce := contract.From(err); ce == nil || ce.Code != contract.CurateInvalid || ce.Field != "curate" {
			t.Fatalf("%s: want CURATE_INVALID, got %v", name, err)
		}
	}
	if _, err := engine.LoadCurate(filepath.Join(t.TempDir(), "missing.json")); contract.From(err).Code != contract.BadFlag {
		t.Fatalf("missing file: %v", err)
	}
}

// Review Focus 3 (against the feature).
func TestResolveGroups(t *testing.T) {
	t.Parallel()
	// b and c share the 7-character prefix "bbbbbbb" and nothing longer.
	a, b, c := strings.Repeat("a", 40), "bbbbbbb1"+strings.Repeat("0", 32), "bbbbbbb2"+strings.Repeat("0", 32)
	sources := []string{a, b, c}
	got, err := engine.ResolveGroups([]plan.Group{{From: []string{c[:12], a}, Message: "x"}, {From: []string{b}, Message: "y"}}, sources)
	if err != nil || !reflect.DeepEqual(got[0].From, []string{c, a}) || got[1].From[0] != b {
		t.Fatalf("resolve = %+v %v", got, err)
	}
	_, err = engine.ResolveGroups([]plan.Group{
		{From: []string{a, a[:7]}, Message: "x"},  // repeated
		{From: []string{"bbbbbbb"}, Message: "y"}, // ambiguous (b and c)
		{From: []string{"1234567"}, Message: "z"}, // unknown
	}, sources)
	ce := contract.From(err)
	if ce == nil || ce.Code != contract.CurateInvalid ||
		!reflect.DeepEqual(ce.Details["repeated"], []string{a}) || !reflect.DeepEqual(ce.Details["ambiguous"], []string{"bbbbbbb"}) ||
		!reflect.DeepEqual(ce.Details["unknown"], []string{"1234567"}) || !reflect.DeepEqual(ce.Details["missing"], []string{b, c}) {
		t.Fatalf("details = %v (%v)", ce.Details, err)
	}
}

// curatedFeature: a.txt "1" then "2" (two commits), and b.txt (one commit).
func curatedFeature(t *testing.T) (*harness, []string) {
	t.Helper()
	h := newHarness(t)
	h.init()
	h.setConfig("default_mode: preserve", "default_mode: curated")
	s1 := h.startFeature("feat-x", map[string]string{"a.txt": "1\n"})
	sg := h.shadowGit()
	s2 := testutil.Commit(t, sg, "feat-x", "b", map[string]string{"b.txt": "b\n"})
	s3 := testutil.Commit(t, sg, "feat-x", "a again", map[string]string{"a.txt": "2\n"})
	return h, []string{s1, s2, s3}
}

func TestCuratedGroupsAndRewords(t *testing.T) {
	t.Parallel()
	h, s := curatedFeature(t)
	file := writeCurate(t, `{"commits": [
		{"from": ["`+s[0]+`", "`+s[2]+`"], "message": "feat: a"},
		{"from": ["`+s[1][:10]+`"], "message": "feat: b"}]}`)
	res, err := promote(h, engine.PromoteOptions{Curate: file, Title: "a and b", Yes: true})
	if err != nil || res.Plan.Mode != "curated" || len(res.Plan.Publish.Commits) != 2 {
		t.Fatalf("promote = %+v %v", res.Plan, err)
	}
	c := res.Plan.Publish.Commits
	if c[0].Subject != "feat: a" || !reflect.DeepEqual(c[0].Sources, []string{s[0], s[2]}) || c[1].Sources[0] != s[1] ||
		!reflect.DeepEqual(res.Plan.Curate[1].From, []string{s[1]}) {
		t.Fatalf("commits = %+v curate = %+v", c, res.Plan.Curate)
	}
	tipSHA, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/feat-x")
	a, _, _ := h.pubGit.ShowFile(h.ctx, c[0].Commit, "a.txt")
	if tipSHA != c[1].Commit || string(a) != "2\n" {
		t.Fatalf("first public commit must hold a.txt 2: %q", a)
	}
}

// Review Focus 4: a reorder whose replay is clean but ends on another tree.
// Public main has x=1. A sets x=2 and adds y; B sets x back to 1. In order
// the feature ends at x=1. B first is clean (base 2, ours 1, theirs 1) and
// empty; A then makes x=2, so the replay ends at x=2 ≠ the gated x=1.
func TestCuratedTreeMismatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "main", "x", map[string]string{"x.txt": "1\n"})
	h.init()
	h.setConfig("default_mode: preserve", "default_mode: curated")
	a := h.startFeature("feat-x", map[string]string{"x.txt": "2\n", "y.txt": "y\n"})
	b := testutil.Commit(t, h.shadowGit(), "feat-x", "back", map[string]string{"x.txt": "1\n"})
	file := writeCurate(t, `{"commits": [{"from": ["`+b+`"], "message": "b"}, {"from": ["`+a+`"], "message": "a"}]}`)
	_, err := promote(h, engine.PromoteOptions{Curate: file, Title: "t", PlanOnly: true})
	ce := wantCode(t, err, contract.CurateTreeMismatch)
	if ce.Details["gated_tree"] == ce.Details["tree"] {
		t.Fatalf("details = %v", ce.Details)
	}
	if h.publicRefs()["refs/heads/feat-x"] != "" {
		t.Fatal("nothing may be published")
	}
}

func TestCuratedReorderConflict(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setConfig("default_mode: preserve", "default_mode: curated")
	s1 := h.startFeature("feat-x", map[string]string{"a.txt": "a1\n"})
	s2 := testutil.Commit(t, h.shadowGit(), "feat-x", "edit a", map[string]string{"a.txt": "a2\n"})
	file := writeCurate(t, `{"commits": [{"from": ["`+s2+`"], "message": "x"}, {"from": ["`+s1+`"], "message": "y"}]}`)
	_, err := promote(h, engine.PromoteOptions{Curate: file, Title: "t", PlanOnly: true})
	ce := wantCode(t, err, contract.CurateConflict)
	if ce.Details["commit"] != s2 || ce.Details["group"] != 0 {
		t.Fatalf("details = %v", ce.Details)
	}
}

func TestCuratedFlags(t *testing.T) {
	t.Parallel()
	file := writeCurate(t, `{"commits": [{"from": ["abcdef1"], "message": "m"}]}`)
	if err := engine.ValidatePromoteOptions(&engine.PromoteOptions{Feature: "f", Mode: "preserve", Curate: file}); contract.From(err).Field != "curate" {
		t.Fatalf("--curate with preserve: %v", err)
	}
	if err := engine.ValidatePromoteOptions(&engine.PromoteOptions{Feature: "f", Mode: "curated"}); contract.From(err).Code != contract.MissingInput {
		t.Fatalf("curated without --curate: %v", err)
	}
}

func TestApplyReplaysCuratedAndRefusesAChangedGroup(t *testing.T) {
	t.Parallel()
	h, s := curatedFeature(t)
	file := writeCurate(t, `{"commits": [{"from": ["`+s[0]+`", "`+s[2]+`"], "message": "a"}, {"from": ["`+s[1]+`"], "message": "b"}]}`)
	res, err := promote(h, engine.PromoteOptions{Curate: file, Title: "t", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	pl := *res.Plan
	pl.Curate = []plan.Group{{From: []string{s[0], s[1], s[2]}, Message: "all"}}
	_, _ = plan.Save(h.eng.WS.PlansDir(), pl)
	_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pl.ID, Yes: true})
	_ = wantCode(t, err, contract.PlanStale)
}

// A plan's groups must name each gated commit once, by full SHA, and only a
// curated plan has groups.
func TestApplyRefusesCurateGroupsThatDoNotFit(t *testing.T) {
	t.Parallel()
	h, s := curatedFeature(t)
	file := writeCurate(t, `{"commits": [{"from": ["`+s[0]+`", "`+s[2]+`"], "message": "a"}, {"from": ["`+s[1]+`"], "message": "b"}]}`)
	res, err := promote(h, engine.PromoteOptions{Curate: file, Title: "t", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for name, groups := range map[string][]plan.Group{
		"omits a commit":   {{From: []string{s[0], s[2]}, Message: "a"}},
		"repeats one":      {{From: []string{s[0], s[2]}, Message: "a"}, {From: []string{s[1], s[0]}, Message: "b"}},
		"short sha":        {{From: []string{s[0], s[2]}, Message: "a"}, {From: []string{s[1][:10]}, Message: "b"}},
		"an empty group":   {{From: []string{s[0], s[2]}, Message: "a"}, {From: []string{s[1]}, Message: "b"}, {From: []string{}, Message: "c"}},
		"no groups at all": nil,
	} {
		pl := *res.Plan
		pl.Curate = groups
		_, _ = plan.Save(h.eng.WS.PlansDir(), pl)
		_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pl.ID, Yes: true})
		if ce := wantCode(t, err, contract.PlanStale); !reflect.DeepEqual(ce.Details["moved"], []string{"curate"}) {
			t.Fatalf("%s: details = %v", name, ce.Details)
		}
	}
	if h.publicRefs()["refs/heads/feat-x"] != "" {
		t.Fatal("nothing may be published")
	}
}

// publicChain is the public commits from main to the published branch,
// oldest first, with each one's message and paths.
func publicChain(t *testing.T, h *harness, branch string) (msgs []string, paths [][]string) {
	t.Helper()
	pm, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/main")
	c, err := h.pubGit.MustResolve(h.ctx, "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("public branch %s: %v", branch, err)
	}
	for c != pm {
		msg, _ := h.pubGit.CommitMessage(h.ctx, c)
		es, _ := h.pubGit.ListTree(h.ctx, c)
		var ps []string
		for _, e := range es {
			ps = append(ps, e.Path)
		}
		msgs, paths = append([]string{msg}, msgs...), append([][]string{ps}, paths...)
		if c, err = h.pubGit.MustResolve(h.ctx, c+"^"); err != nil {
			t.Fatal(err)
		}
	}
	return msgs, paths
}

// A reorder whose intermediate group holds a secret the final tree no
// longer has: the curated gate passes on the final tree, but promote scans
// every curated commit and publishes nothing.
func TestCuratedScansEveryGroupTree(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setConfig("default_mode: preserve", "default_mode: curated")
	s1 := h.startFeature("feat-x", map[string]string{"a.txt": "a\n"})
	s2 := testutil.Commit(t, h.shadowGit(), "feat-x", "add config", map[string]string{"config.env": "TOKEN=" + testutil.FakeGitHubToken() + "\n"})
	s3 := testutil.Commit(t, h.shadowGit(), "feat-x", "drop config", map[string]string{"config.env": testutil.Delete})
	file := writeCurate(t, `{"commits": [{"from": ["`+s2+`"], "message": "config"}, {"from": ["`+s1+`", "`+s3+`"], "message": "a"}]}`)
	before := h.publicRefs()
	res, err := promote(h, engine.PromoteOptions{Curate: file, Title: "t", Yes: true})
	ce := wantCode(t, err, contract.LeakFound)
	if ce.Details["mode"] != "curated" || res.Gate == nil || res.Gate.ScannedAs != "curated" || res.Gate.Leak != "clean" ||
		len(res.Findings) != 1 || res.Findings[0].Path != "config.env" || res.Plan != nil {
		t.Fatalf("promote = %+v details = %v", res, ce.Details)
	}
	if !reflect.DeepEqual(h.publicRefs(), before) {
		t.Fatal("nothing may be published")
	}
}

// An overlay path a middle group adds is stripped from that public commit
// too, and the chain still passes assertChain (promote and apply).
func TestCuratedStripsOverlayPathsFromEveryCommit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setConfig("default_mode: preserve", "default_mode: curated")
	s1 := h.startFeature("feat-x", map[string]string{"a.txt": "a\n"})
	s2 := testutil.Commit(t, h.shadowGit(), "feat-x", "notes", map[string]string{"b.txt": "b\n", ".ghostbranch/notes.md": "private\n"})
	s3 := testutil.Commit(t, h.shadowGit(), "feat-x", "c", map[string]string{"c.txt": "c\n", ".ghostbranch/notes.md": testutil.Delete})
	file := writeCurate(t, `{"commits": [{"from": ["`+s1+`"], "message": "a"}, {"from": ["`+s2+`"], "message": "b"}, {"from": ["`+s3+`"], "message": "c"}]}`)
	res, err := promote(h, engine.PromoteOptions{Curate: file, Title: "t", Yes: true})
	if err != nil || res.Applied == nil || len(res.Plan.Publish.Commits) != 3 {
		t.Fatalf("promote = %+v %v", res, err)
	}
	_, paths := publicChain(t, h, "feat-x")
	if len(paths) != 3 || !slices.Contains(paths[1], "b.txt") {
		t.Fatalf("public paths = %v", paths)
	}
	for i, ps := range paths {
		for _, p := range ps {
			if strings.HasPrefix(p, ".ghostbranch/") {
				t.Fatalf("public commit %d holds overlay path %s", i, p)
			}
		}
	}
}

// The whole curated flow from disk: plan, load the plan by id, apply it at
// a console; the public branch has the curated commits in order.
func TestCuratedPlanFromDiskApplies(t *testing.T) {
	t.Parallel()
	h, s := curatedFeature(t)
	file := writeCurate(t, `{"commits": [{"from": ["`+s[1]+`"], "message": "feat: b\n\nwhy b"}, {"from": ["`+s[0]+`", "`+s[2]+`"], "message": "feat: a"}]}`)
	res, err := promote(h, engine.PromoteOptions{Curate: file, Title: "a and b", PlanOnly: true})
	if err != nil || res.PlanPath == "" {
		t.Fatalf("promote --plan = %+v %v", res, err)
	}
	pl, _, err := plan.Load(h.eng.WS.PlansDir(), res.Plan.ID)
	if err != nil || pl.Mode != "curated" || len(pl.Curate) != 2 {
		t.Fatalf("plan.Load = %+v %v", pl, err)
	}
	ar, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pl.ID})
	if err != nil || ar.PublicPR != 1 || len(h.approver.asked) != 1 {
		t.Fatalf("apply = %+v %v (asked %d)", ar, err, len(h.approver.asked))
	}
	msgs, paths := publicChain(t, h, "feat-x")
	if !reflect.DeepEqual(msgs, []string{"feat: b\n\nwhy b\n", "feat: a\n"}) || !reflect.DeepEqual(paths[0], []string{".github/workflows/ci.yml", "README.md", "b.txt"}) {
		t.Fatalf("public chain = %q %v", msgs, paths)
	}
	tip, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/feat-x")
	if a, _, _ := h.pubGit.ShowFile(h.ctx, tip, "a.txt"); tip != pl.Publish.Commit || string(a) != "2\n" {
		t.Fatalf("tip %s (plan %s) a.txt %q", tip, pl.Publish.Commit, a)
	}
}

// A conflict in a later group names that group and its source commit.
func TestCuratedConflictInALaterGroup(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setConfig("default_mode: preserve", "default_mode: curated")
	s1 := h.startFeature("feat-x", map[string]string{"a.txt": "a1\n"})
	s2 := testutil.Commit(t, h.shadowGit(), "feat-x", "b", map[string]string{"b.txt": "b\n"})
	s3 := testutil.Commit(t, h.shadowGit(), "feat-x", "edit a", map[string]string{"a.txt": "a2\n"})
	file := writeCurate(t, `{"commits": [{"from": ["`+s2+`"], "message": "b"}, {"from": ["`+s3+`"], "message": "x"}, {"from": ["`+s1+`"], "message": "y"}]}`)
	_, err := promote(h, engine.PromoteOptions{Curate: file, Title: "t", PlanOnly: true})
	ce := wantCode(t, err, contract.CurateConflict)
	if ce.Details["commit"] != s3 || ce.Details["group"] != 1 {
		t.Fatalf("details = %v", ce.Details)
	}
}
