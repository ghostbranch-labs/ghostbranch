package cli

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli/style"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
)

func TestGateCell(t *testing.T) {
	t.Parallel()
	job := "gate/feat-x/0123456789ab"
	for _, c := range []struct {
		g    *engine.GateSummary
		want string
	}{
		{nil, "-"},
		{&engine.GateSummary{Checks: "pending", Leak: "clean", Fresh: true, Job: &job}, "pending (job " + job + ")"},
		{&engine.GateSummary{Checks: "pending", Leak: "clean", Fresh: false, Job: &job}, "pending (job " + job + ") (stale)"},
		{&engine.GateSummary{Checks: "passed", Leak: "clean", Fresh: true}, "passed"},
		{&engine.GateSummary{Checks: "passed", Leak: "not_run", Fresh: true}, "passed, scan not_run"},
		{&engine.GateSummary{Checks: "failed", Leak: "not_run", Fresh: false}, "failed, scan not_run (stale)"},
		{&engine.GateSummary{Checks: "not_run", Leak: "not_run", Fresh: true}, "not_run"},
		{&engine.GateSummary{Checks: "failed", Leak: "found", Fresh: true}, "leak"},
	} {
		if got := gateCell(c.g); got != c.want {
			t.Errorf("gateCell(%+v) = %q, want %q", c.g, got, c.want)
		}
	}
}

// tableNames is the NAME column of featureTable's rows. The cell is
// everything before the STATE column; tabwriter pads by runes, so it cuts
// by runes ("└─" is more than one byte).
func tableNames(out string) []string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	col := strings.Index(lines[0], "STATE")
	var names []string
	for _, line := range lines[1:] {
		names = append(names, strings.TrimRight(string([]rune(line)[:col]), " "))
	}
	return names
}

func TestFeatureTableShowsStacksAsTrees(t *testing.T) {
	t.Parallel()
	n := 4
	rs := []engine.FeatureReport{
		{Name: "a", State: "published", Parent: "main", Children: []string{"b"}, Drift: engine.Drift{Status: "clean"}, Stack: &n},
		{Name: "b", State: "published", Parent: "a", Children: []string{"c", "d"}, Drift: engine.Drift{Status: "clean"}, Stack: &n},
		{Name: "c", State: "parent_stale", Parent: "b", Children: []string{}, Drift: engine.Drift{Status: "clean"}},
		{Name: "d", State: "draft", Parent: "b", Children: []string{}, Drift: engine.Drift{Status: "clean"}},
		{Name: "solo", State: "draft", Parent: "main", Children: []string{}, Drift: engine.Drift{Status: "clean"}},
	}
	var b strings.Builder
	featureTable(&b, rs)
	want := []string{"a", "└─ b", "  └─ c", "  └─ d", "solo"}
	if names := tableNames(b.String()); !reflect.DeepEqual(names, want) {
		t.Fatalf("NAME column = %q, want %q\n%s", names, want, b.String())
	}
	if !strings.Contains(b.String(), "stack #4") || !strings.Contains(b.String(), "parent_stale") {
		t.Fatalf("the table must show the stack and the state:\n%s", b.String())
	}
}

// Records whose parents loop have no root; every feature is still listed,
// once.
func TestFeatureTableListsALoopFlat(t *testing.T) {
	t.Parallel()
	rs := []engine.FeatureReport{
		{Name: "x", State: "draft", Parent: "y", Drift: engine.Drift{Status: "clean"}},
		{Name: "y", State: "draft", Parent: "x", Drift: engine.Drift{Status: "clean"}},
		{Name: "z", State: "unreadable", Parent: "", Drift: engine.Drift{Status: "unknown"}},
	}
	var b strings.Builder
	featureTable(&b, rs)
	if names, want := tableNames(b.String()), []string{"z", "x", "└─ y"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("NAME column = %q, want %q\n%s", names, want, b.String())
	}
}

// feature show names the branch below and the branches on it, only for a
// feature in a stack.
func TestFeatureShowTextNamesTheStack(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		r    engine.FeatureReport
		want string
	}{
		{engine.FeatureReport{Name: "b", Parent: "a", Children: []string{"c", "d"}}, "  stack: on a; branches on it: c, d\n"},
		{engine.FeatureReport{Name: "c", Parent: "b", Children: []string{}}, "  stack: on b; branches on it: none\n"},
		{engine.FeatureReport{Name: "a", Parent: "main", Children: []string{"b"}}, "  stack: on main; branches on it: b\n"},
		{engine.FeatureReport{Name: "x", Parent: "", Children: []string{"y"}}, "  stack: on unknown; branches on it: y\n"},
		{engine.FeatureReport{Name: "solo", Parent: "main", Children: []string{}}, ""},
	} {
		var b strings.Builder
		featureShowText(&b, &engine.FeatureShowResult{Feature: c.r})
		has := strings.Contains(b.String(), "  stack: ")
		if (c.want == "" && has) || (c.want != "" && !strings.Contains(b.String(), c.want)) {
			t.Errorf("%s: feature show =\n%s\nwant the line %q", c.r.Name, b.String(), c.want)
		}
	}
}

// M5/R13: an applied update says what happened to the open PR, not that a
// PR was published.
func TestHumanSaysHowAnOpenPRWasUpdated(t *testing.T) {
	t.Parallel()
	url := "https://github.example/acme/widget/pull/3"
	applied := func(style string) *engine.ApplyResult {
		prs := []plan.StackPR{{Feature: "feat-x", Base: "main", Number: 3, URL: url}}
		if style != "" {
			prs[0].Update, prs[0].Style = true, style
		}
		return &engine.ApplyResult{PublicBranch: "feat-x", PublicPR: 3, PRURL: url, PRs: prs}
	}
	commits := func(n int) *plan.Plan { return &plan.Plan{Publish: plan.Publish{Commits: make([]plan.Commit, n)}} }
	for _, c := range []struct {
		r    any
		want string
	}{
		{&engine.PromoteResult{Applied: applied(plan.StyleAppend), Plan: commits(1)}, "Updated PR #3 (added 1 commit): " + url + "\n"},
		{&engine.PromoteResult{Applied: applied(plan.StyleAppend), Plan: commits(2)}, "Updated PR #3 (added 2 commits): " + url + "\n"},
		{&engine.PromoteResult{Applied: applied(plan.StyleReplace), Plan: commits(1)}, "Replaced PR #3's branch feat-x: " + url + "\n"},
		{&engine.PromoteResult{Applied: applied(""), Plan: commits(1)}, "Published feat-x as PR #3: " + url + "\n"},
		{applied(plan.StyleAppend), "Updated PR #3 on feat-x: " + url + "\n"},
		{applied(plan.StyleReplace), "Replaced PR #3's branch feat-x: " + url + "\n"},
		{applied(""), "Published feat-x as PR #3: " + url + "\n"},
	} {
		if got := human(c.r, style.Styler{}); !strings.HasPrefix(got, c.want) {
			t.Errorf("human(%T) = %q, want it to start %q", c.r, got, c.want)
		}
	}
	// A stack update names each PR and what happened to it; the top's open
	// PR was updated, never "published".
	u1, u2 := "https://github.example/acme/widget/pull/13", "https://github.example/acme/widget/pull/14"
	stack := &engine.ApplyResult{PublicBranch: "b", PublicPR: 14, PRURL: u2, PRs: []plan.StackPR{
		{Feature: "a", Base: "main", Number: 13, URL: u1, Update: true, Style: plan.StyleReplace},
		{Feature: "b", Base: "a", Number: 14, URL: u2, Update: true, Style: plan.StyleReplace}}}
	want := "Replaced PR #13's branch a: " + u1 + "\nReplaced PR #14's branch b: " + u2 + "\n"
	for _, r := range []any{stack, &engine.PromoteResult{Applied: stack, Plan: commits(1)}} {
		if got := human(r, style.Styler{}); !strings.HasPrefix(got, want) || strings.Contains(got, "Published") {
			t.Errorf("human(%T) for a stack update = %q, want it to start %q", r, got, want)
		}
	}
	// One appended to, one opened on it.
	stack.PRs = []plan.StackPR{{Feature: "a", Base: "main", Number: 13, URL: u1, Update: true, Style: plan.StyleAppend},
		{Feature: "b", Base: "a", Number: 14, URL: u2}}
	want = "Updated PR #13 on a: " + u1 + "\nPublished b as PR #14: " + u2 + "\n"
	if got := human(stack, style.Styler{}); !strings.HasPrefix(got, want) {
		t.Errorf("human for an append and a first publish = %q, want it to start %q", got, want)
	}
}

// F-13: a restack dry run that stops on a conflict says where it stops,
// and never that it would do nothing.
func TestRestackPlanConflictNeverSaysNothing(t *testing.T) {
	t.Parallel()
	r := &engine.RestackResult{Feature: "feat-x", Restacked: []engine.RestackedBranch{}, Pending: []string{"feat-y"},
		Preview: preview.New(map[string]string{}), StoppedOnConflict: true}
	want := "Dry run; nothing was changed.\nIt would stop at feat-y: it doesn't restack cleanly (see the error below).\n"
	if got := human(r, style.Styler{}); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	r.Pending = []string{"feat-y", "feat-z"}
	r.Preview.Add(preview.PushRef("acme/widget-shadow", "refs/heads/feat-w", "a", "b"))
	got := human(r, style.Styler{})
	if !strings.Contains(got, "Before stopping, it would:\n  - ") || !strings.Contains(got, "It would stop at feat-y:") || strings.Contains(got, "(nothing)") {
		t.Fatalf("got %q", got)
	}
}

// A restack dry run that stopped on anything but a conflict (Ctrl-C, a git
// failure) says only where it stopped, not that the branch conflicts.
func TestRestackPlanOtherStopNeverClaimsAConflict(t *testing.T) {
	t.Parallel()
	r := &engine.RestackResult{Feature: "feat-x", Restacked: []engine.RestackedBranch{}, Pending: []string{"feat-y"},
		Preview: preview.New(map[string]string{})}
	want := "Dry run; nothing was changed.\nIt stopped at feat-y (see the error below).\n"
	if got := human(r, style.Styler{}); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The general guard: after an error, no preview says "(nothing)".
func TestWithoutNothingAfterAnError(t *testing.T) {
	t.Parallel()
	out := "Dry run; nothing was changed. This would:\n" + nothingLine
	// No header left dangling over an empty list either.
	if got := withoutNothing(out, true); got != "Dry run; nothing was changed.\n" {
		t.Fatalf("failed: %q", got)
	}
	if got := withoutNothing(out, false); got != out {
		t.Fatalf("succeeded: %q", got)
	}
}

// render applies the guard: a failed command whose result carries an empty
// preview prints neither "(nothing)" nor a dangling "This would:".
func TestRenderOfAFailedDryRunNeverSaysNothing(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	a := &app{env: Env{Stdout: &out, Stderr: &errOut, Getenv: func(string) string { return "" }}, command: "feature restack",
		result: &engine.RestackResult{Feature: "feat-x", Restacked: []engine.RestackedBranch{}, Pending: []string{},
			Preview: preview.New(map[string]string{})}}
	a.render(contract.New(contract.Internal, "git went away"))
	if got := out.String(); strings.Contains(got, "(nothing)") || strings.Contains(got, "This would:") {
		t.Fatalf("stdout: %q", got)
	}
	if !strings.Contains(errOut.String(), "[INTERNAL]") {
		t.Fatalf("stderr: %q", errOut.String())
	}
}

// F-23: an exit code's header says "exit 5:" once; a code keeps its kind
// and exit code in brackets.
func TestExplainTextHeaders(t *testing.T) {
	t.Parallel()
	exit, err := contract.Explain("5")
	if err != nil {
		t.Fatal(err)
	}
	got := human(&ExplainResult{Entries: exit}, style.Styler{})
	if !strings.HasPrefix(got, "exit 5: A person has to approve this before it runs") || strings.Contains(got, "(exit,") {
		t.Errorf("explain 5 = %q", got)
	}
	if !strings.Contains(got, "\nAPPROVAL_REQUIRED (error, exit 5): ") {
		t.Errorf("explain 5 lacks its codes: %q", got)
	}
	warn, err := contract.Explain("UPDATE_AVAILABLE")
	if err != nil {
		t.Fatal(err)
	}
	if got := human(&ExplainResult{Entries: warn}, style.Styler{}); !strings.HasPrefix(got, "UPDATE_AVAILABLE (warning): ") {
		t.Errorf("explain UPDATE_AVAILABLE = %q", got)
	}
	// explain --all: every exit code once, in the new form.
	all := human(&ExplainResult{Entries: contract.Registry()}, style.Styler{})
	for x := 0; x <= 6; x++ {
		if !strings.Contains(all, fmt.Sprintf("exit %d: ", x)) {
			t.Errorf("explain --all lacks exit %d", x)
		}
	}
	if strings.Contains(all, "(exit,") {
		t.Errorf("explain --all still says (exit, …)")
	}
}

// F-11: a stack plan's text names every PR with its own commit count, not
// only the bottom's.
func TestStackPlanTextCountsEveryPR(t *testing.T) {
	t.Parallel()
	entry := func(name string, n, dropped int) plan.Entry {
		return plan.Entry{Feature: name, Mode: "preserve",
			Publish: plan.Publish{Commits: make([]plan.Commit, n), DroppedEmpty: make([]string, dropped)}}
	}
	p := &plan.Plan{ID: "20261014T072852Z-563f9d", Mode: "preserve", Feature: "feat-a",
		Publish: plan.Publish{Commits: make([]plan.Commit, 8)},
		Chain:   []plan.Entry{entry("feat-a", 8, 0), entry("feat-b", 39, 2)},
		PRs:     []plan.StackPR{{Feature: "feat-a", Base: "main"}, {Feature: "feat-b", Base: "feat-a"}}}
	want := "Plan 20261014T072852Z-563f9d saved to /plans/p.json: 2 PRs, bottom first.\n" +
		"  feat-a (preserve): 8 public commit(s), PR on main\n" +
		"  feat-b (preserve): 39 public commit(s), 2 dropped as empty, PR on feat-a\n" +
		"Publish it with: ghostbranch apply 20261014T072852Z-563f9d\n"
	if got := human(&engine.PromoteResult{Plan: p, PlanPath: "/plans/p.json"}, style.Styler{}); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	// One feature: the line it always had.
	one := &plan.Plan{ID: "p1", Mode: "squash", Publish: plan.Publish{Commits: make([]plan.Commit, 1)},
		PRs: []plan.StackPR{{Feature: "feat-x", Base: "main"}}}
	want = "Plan p1 saved to /plans/p1.json.\n  squash: 1 public commit(s)\nPublish it with: ghostbranch apply p1\n"
	if got := human(&engine.PromoteResult{Plan: one, PlanPath: "/plans/p1.json"}, style.Styler{}); got != want {
		t.Errorf("one feature: got %q, want %q", got, want)
	}
}

// F-15: a fresh stack apply names every PR it opened, bottom first.
func TestFreshStackApplyNamesEveryPR(t *testing.T) {
	t.Parallel()
	u1, u2 := "https://github.example/acme/widget/pull/13", "https://github.example/acme/widget/pull/14"
	r := &engine.ApplyResult{PublicBranch: "feat-b", PublicPR: 14, PRURL: u2, PRs: []plan.StackPR{
		{Feature: "feat-a", Base: "main", Number: 13, URL: u1}, {Feature: "feat-b", Base: "feat-a", Number: 14, URL: u2}}}
	want := "Published feat-a as PR #13: " + u1 + "\nPublished feat-b as PR #14: " + u2 + "\n"
	for _, res := range []any{r, &engine.PromoteResult{Applied: r, Plan: &plan.Plan{}}} {
		if got := human(res, style.Styler{}); !strings.HasPrefix(got, want) {
			t.Errorf("human(%T) = %q, want it to start %q", res, got, want)
		}
	}
}

// A guided init on a private shadow repo says again, just before the
// walkthrough, that gates bill Actions minutes; otherwise it says nothing.
func TestGuidedInitSaysMinutesAreBilled(t *testing.T) {
	t.Parallel()
	const note = "Note: acme/widget-shadow is private, so the CI that every gate runs bills your GitHub Actions minutes.\n"
	res := func(guided, billed bool) *engine.InitResult {
		return &engine.InitResult{Public: "acme/widget", Shadow: "acme/widget-shadow", Ready: true, Guided: guided,
			Audit: &engine.AuditResult{Shadow: "acme/widget-shadow", Minutes: engine.AuditMinutes{Billed: billed}}}
	}
	got := human(res(true, true), style.Styler{})
	if !strings.Contains(got, "\n"+note+"\nYour first feature:\n") {
		t.Errorf("guided and billed: %q", got)
	}
	for _, c := range []struct{ guided, billed bool }{{true, false}, {false, true}} {
		if got := human(res(c.guided, c.billed), style.Styler{}); strings.Contains(got, "Note:") {
			t.Errorf("guided %v, billed %v: %q", c.guided, c.billed, got)
		}
	}
	// No audit (it didn't run): no note, and the walkthrough still shows.
	noAudit := res(true, true)
	noAudit.Audit = nil
	if got := human(noAudit, style.Styler{}); strings.Contains(got, "Note:") || !strings.Contains(got, "Your first feature:") {
		t.Errorf("guided without an audit: %q", got)
	}
}
