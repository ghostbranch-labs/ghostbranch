package cli

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

func testApp(command string, args ...string) *app {
	return &app{env: Env{Getenv: func(string) string { return "" }}, command: command, args: args,
		pairFull: workspace.Pair{Public: "acme/widget", Shadow: "acme/widget-shadow"}}
}

func TestWithoutSwitch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want []string }{
		{[]string{"promote", "feat-x", "--detach", "--plan"}, []string{"promote", "feat-x", "--plan"}},
		{[]string{"promote", "feat-x", "--detach=true", "--plan"}, []string{"promote", "feat-x", "--plan"}},
		{[]string{"promote", "feat-x", "--detached", "--detach"}, []string{"promote", "feat-x", "--detached"}},
	} {
		if got := withoutSwitch(tc.in, "--detach"); !slices.Equal(got, tc.want) {
			t.Errorf("withoutSwitch(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The job errors a cancel or the lock can leave say how to finish the job.
// (A cancel can't be sent through Run in a test, so next is called here.)
func TestNextForJobErrors(t *testing.T) {
	t.Parallel()
	const job = "gate/feat-x/0123456789ab"
	wait := []string{"ghostbranch", "wait", job, "--repo", "acme/widget"}
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{"interrupted with a job", contract.New(contract.Interrupted, "x").WithDetails(map[string]any{"job": job}), wait},
		{"interrupted before CI", contract.New(contract.Interrupted, "x").WithDetails(map[string]any{"feature": "feat-x"}),
			[]string{"ghostbranch", "gate", "feat-x", "--json"}},
		{"locked after CI", contract.New(contract.Locked, "x").WithDetails(map[string]any{"job": job}), wait},
		{"stale, none current", contract.New(contract.JobStale, "x").WithDetails(map[string]any{"job": job, "current_job": ""}),
			[]string{"ghostbranch", "gate", "feat-x", "--repo", "acme/widget"}},
		{"not found after a job", contract.New(contract.JobNotFound, "x").WithDetails(map[string]any{"job": job}),
			[]string{"ghostbranch", "gate", "feat-x", "--repo", "acme/widget"}},
	} {
		a := &app{env: Env{Getenv: func(string) string { return "" }}, args: []string{"gate", "feat-x", "--json"},
			pairFull: workspace.Pair{Public: "acme/widget", Shadow: "acme/widget-shadow"}}
		next := a.next(tc.err)
		if len(next) != 1 || !slices.Equal(next[0].Command, tc.want) {
			t.Errorf("%s: next = %+v, want %v", tc.name, next, tc.want)
		}
	}
	// A plain LOCKED runs the same command again (F-29).
	a := &app{env: Env{Getenv: func(string) string { return "" }}, args: []string{"sync"}}
	if next := a.next(contract.New(contract.Locked, "x")); len(next) != 1 || !slices.Equal(next[0].Command, []string{"ghostbranch", "sync"}) {
		t.Errorf("a plain LOCKED repeats the command: %+v", next)
	}
	// An interrupted promote --detach: wait on the job, then promote again
	// without --detach, which reuses the green gate.
	a = &app{env: Env{Getenv: func(string) string { return "" }}, command: "promote",
		args:     []string{"promote", "feat-x", "--detach", "--plan", "--json"},
		pairFull: workspace.Pair{Public: "acme/widget", Shadow: "acme/widget-shadow"}}
	next := a.next(contract.New(contract.Interrupted, "x").WithDetails(map[string]any{"job": job}))
	if len(next) != 2 || !slices.Equal(next[0].Command, wait) ||
		!slices.Equal(next[1].Command, []string{"ghostbranch", "promote", "feat-x", "--plan", "--json"}) {
		t.Errorf("interrupted promote: next = %+v", next)
	}
}

// A promote interrupted after its plan was saved resumes with apply <plan>,
// and a public branch at an earlier plan's commits names that plan: making
// new commits would only end in BRANCH_EXISTS.
func TestNextPointsAtTheSavedPlan(t *testing.T) {
	t.Parallel()
	const id = "20261009T120000Z-abcdef"
	apply := []string{"ghostbranch", "apply", id}
	a := &app{env: Env{Getenv: func(string) string { return "" }}, command: "promote",
		args:     []string{"promote", "feat-x", "--mode", "squash", "-m", "Add x", "--json"},
		pairFull: workspace.Pair{Public: "acme/widget", Shadow: "acme/widget-shadow"}}
	next := a.next(contract.New(contract.Interrupted, "x").WithDetails(map[string]any{"plan_id": id, "feature": "feat-x"}))
	if len(next) != 1 || !slices.Equal(next[0].Command, apply) {
		t.Errorf("interrupted after the plan: next = %+v, want %v", next, apply)
	}
	a.command, a.args = "apply", []string{"apply", "20261009T130000Z-123456", "--json"}
	next = a.next(contract.New(contract.BranchExists, "x").WithDetails(map[string]any{"branch": "feat-x", "sha": "abc", "plan_id": id}))
	if len(next) == 0 || !slices.Equal(next[0].Command, apply) {
		t.Errorf("branch at an earlier plan: next = %+v, want %v first", next, apply)
	}
	if next := a.next(contract.New(contract.BranchExists, "x").WithDetails(map[string]any{"branch": "feat-x", "sha": "abc"})); len(next) != 0 {
		t.Errorf("branch at another commit: next = %+v, want none", next)
	}
}

// A merged feature whose branch is gone but whose gate is past what was
// published isn't closed by sync: a person closes it at the console.
func TestNextForAMergedBranchGoneFeatureSyncLeaves(t *testing.T) {
	t.Parallel()
	a := &app{env: Env{Getenv: func(string) string { return "" }}, command: "status",
		pairFull: workspace.Pair{Public: "acme/widget", Shadow: "acme/widget-shadow"}}
	r := engine.FeatureReport{Name: "feat-x", State: feature.StateBranchGone, PRState: "merged"}
	if engine.ClosableWithoutConsole(r) {
		t.Fatal("a report the survey didn't tie to its publish must not be closable unattended")
	}
	next := a.featureNexts(nil, []engine.FeatureReport{r})
	want := []string{"ghostbranch", "feature", "close", "feat-x", "--repo", "acme/widget"}
	if len(next) != 1 || !slices.Equal(next[0].Command, want) || !next[0].NeedsHuman {
		t.Fatalf("next = %+v, want %v for a person", next, want)
	}
}

func TestWithoutFlagsKnowsWhichFlagsTakeValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in, drop, want []string
	}{
		// A value that looks like a named flag is another flag's value.
		{[]string{"promote", "f", "--title", "--curate", "--curate", "c.json", "--plan"}, []string{"--curate"},
			[]string{"promote", "f", "--title", "--curate", "--plan"}},
		// -m's prefix match no longer eats another flag's value.
		{[]string{"promote", "f", "--title", "-mumble", "-m", "msg"}, []string{"-m", "--message"},
			[]string{"promote", "f", "--title", "-mumble"}},
		{[]string{"promote", "f", "-mmsg", "--message=x", "-m=y", "--plan"}, []string{"-m", "--message"},
			[]string{"promote", "f", "--plan"}},
		{[]string{"promote", "f", "--body=b", "--title", "t"}, []string{"--title"}, []string{"promote", "f", "--body=b"}},
		{[]string{"promote", "f", "--", "--title", "x"}, []string{"--title"}, []string{"promote", "f", "--", "--title", "x"}},
	} {
		if got := withoutFlags(tc.in, tc.drop...); !slices.Equal(got, tc.want) {
			t.Errorf("withoutFlags(%v, %v) = %v, want %v", tc.in, tc.drop, got, tc.want)
		}
	}
	if v, ok := flagValue([]string{"promote", "f", "--title", "--body", "--body=b"}, "--body"); !ok || v != "b" {
		t.Errorf("flagValue --body = %q %v", v, ok)
	}
	if got := positionals([]string{"--repo", "x", "apply", "--json", "p.json"}); !slices.Equal(got, []string{"apply", "p.json"}) {
		t.Errorf("positionals = %v", got)
	}
}

// valueFlags lists exactly the flags that take a value, by every name.
// Reflection reads the flags, so pflag (an indirect dependency) stays out
// of the imports.
func TestValueFlagsMatchTheCommands(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		visit := reflect.ValueOf(c.LocalFlags()).MethodByName("VisitAll")
		visit.Call([]reflect.Value{reflect.MakeFunc(visit.Type().In(0), func(args []reflect.Value) []reflect.Value {
			f := args[0].Elem()
			takes := f.FieldByName("Value").Interface().(interface{ Type() string }).Type() != "bool"
			for _, name := range []string{"--" + f.FieldByName("Name").String(), "-" + f.FieldByName("Shorthand").String()} {
				if name == "-" {
					continue
				}
				seen[name] = true
				if valueFlags[name] != takes {
					t.Errorf("%s %s: valueFlags says %v, the flag takes a value: %v", c.CommandPath(), name, valueFlags[name], takes)
				}
			}
			return nil
		})})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk((&app{}).root())
	for name := range valueFlags {
		if !seen[name] {
			t.Errorf("valueFlags lists %s, which no command has", name)
		}
	}
}

// The suggested squash or curated promote keeps the --branch, --title and
// --body the promote was given, except a text a finding is in; for apply,
// the plan's public branch.
func TestNextKeepsTheTextsInAHistoryPromote(t *testing.T) {
	t.Parallel()
	a := testApp("promote", "promote", "feat-x", "--mode", "preserve", "--title", "T", "--body", "B", "--branch", "pub-x", "--plan", "--json")
	a.result = &engine.PromoteResult{Gate: &engine.GateResult{Commits: []engine.CommitInfo{{Source: "s1"}, {Source: "s2"}}, Leak: feature.LeakFound}}
	older := []leak.Finding{{Where: "tree", Path: "a.txt", Commits: []string{"s1"}}}
	next := a.next(contract.New(contract.LeakFound, "x").WithDetails(map[string]any{"feature": "feat-x", "mode": "preserve", "findings": older}))
	keep := []string{"--branch", "pub-x", "--title", "T", "--body", "B", "--repo", "acme/widget"}
	if len(next) < 2 || !slices.Equal(next[0].Command, append([]string{"ghostbranch", "promote", "feat-x", "--mode", "squash", "--plan"}, keep...)) ||
		!slices.Equal(next[1].Command, append([]string{"ghostbranch", "promote", "feat-x", "--mode", "curated", "--plan"}, keep...)) ||
		!slices.Equal(next[1].NeedsInput, []string{"--curate"}) {
		t.Fatalf("next = %+v", next)
	}
	withTitle := append(older, leak.Finding{Where: "pr_title"})
	next = a.next(contract.New(contract.LeakFound, "x").WithDetails(map[string]any{"feature": "feat-x", "mode": "preserve", "findings": withTitle}))
	for _, n := range next {
		if slices.Contains(n.Command, "T") {
			t.Fatalf("a leaking title was kept: %+v", n)
		}
	}
	a = testApp("apply", "apply", "20261009T120000Z-abcdef", "--json")
	next = a.next(contract.New(contract.LeakFound, "x").WithDetails(map[string]any{"feature": "feat-x", "mode": "preserve",
		"public": "acme/widget", "branch": "pub-x", "findings": older}))
	if len(next) == 0 || !slices.Equal(next[0].Command, []string{"ghostbranch", "promote", "feat-x", "--mode", "squash", "--plan", "--branch", "pub-x", "--repo", "acme/widget"}) {
		t.Fatalf("apply next = %+v", next)
	}
}

// A curated promote's messages come from its --curate file: with an older
// tree finding too, the rewrite step doesn't say to reword the commits.
func TestNextForACurateMessageAndAnOlderTreeFinding(t *testing.T) {
	t.Parallel()
	a := testApp("promote", "promote", "feat-x", "--mode", "curated", "--curate", "c.json", "--title", "T", "--plan")
	a.result = &engine.PromoteResult{Gate: &engine.GateResult{Commits: []engine.CommitInfo{{Source: "s1"}, {Source: "s2"}}, Leak: feature.LeakClean}}
	fs := []leak.Finding{{Where: "commit_message", Commits: []string{"s1"}}, {Where: "tree", Path: "a.txt", Commits: []string{"s1"}}}
	next := a.next(contract.New(contract.LeakFound, "x").WithDetails(map[string]any{"feature": "feat-x", "mode": "curated", "findings": fs}))
	saysCurate := false
	for _, n := range next {
		if strings.Contains(n.Why, "reword") {
			t.Fatalf("the messages came from the --curate file: %+v", n)
		}
		saysCurate = saysCurate || strings.Contains(n.Why, "--curate file whose messages don't leak")
	}
	if !saysCurate {
		t.Fatalf("next = %+v", next)
	}
}

// A finding the tip introduced is amended away, not fixed on top, when the
// mode publishes each commit; under squash the plain fix is enough.
func TestNextAmendsForAFindingTheTipIntroduced(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		mode, want string
	}{{"preserve", "amend"}, {"squash", "after fixing the content"}} {
		a := testApp("gate", "gate", "feat-x")
		a.shadowRemote = "shadow"
		a.result = &engine.GateResult{Commits: []engine.CommitInfo{{Source: "s1"}}, Leak: feature.LeakFound}
		var commits []string
		if c.mode != "squash" {
			commits = []string{"s1"}
		}
		next := a.next(contract.New(contract.LeakFound, "x").WithDetails(map[string]any{"feature": "feat-x", "mode": c.mode,
			"findings": []leak.Finding{{Where: "tree", Path: "a.txt", Commits: commits}}}))
		if len(next) == 0 || !strings.Contains(next[0].Why, c.want) {
			t.Errorf("%s: next = %+v", c.mode, next)
		}
	}
}

// Without a gate result (apply), details.feature_head is the tip: a
// finding at the tip isn't treated as one only in older commits.
func TestNextWithoutAGateResultUsesTheFeatureHead(t *testing.T) {
	t.Parallel()
	d := func(head string) map[string]any {
		m := map[string]any{"feature": "feat-x", "public": "acme/widget", "mode": "preserve", "branch": "feat-x",
			"findings": []leak.Finding{{Where: "tree", Path: "a.txt", Commits: []string{"tip"}}}}
		if head != "" {
			m["feature_head"] = head
		}
		return m
	}
	a := testApp("apply", "apply", "20261009T120000Z-abcdef")
	next := a.next(contract.New(contract.LeakFound, "x").WithDetails(d("tip")))
	if len(next) != 1 || !strings.Contains(next[0].Why, "make a new plan") {
		t.Fatalf("a tip finding: next = %+v", next)
	}
	if next := a.next(contract.New(contract.LeakFound, "x").WithDetails(d(""))); len(next) == 0 || next[0].Command[4] != "squash" {
		t.Fatalf("with the tip unknown, the history steps stay: %+v", next)
	}
}

// promote's own chain hit an older commit's broken marker: with
// details.feature it gets the history steps, not only "run this again".
func TestNextForAPromoteChainMarker(t *testing.T) {
	t.Parallel()
	a := testApp("promote", "promote", "feat-x", "--mode", "preserve", "--title", "t", "--plan")
	a.result = &engine.PromoteResult{Gate: &engine.GateResult{Commits: []engine.CommitInfo{{Source: "s1"}, {Source: "s2"}}, Leak: feature.LeakClean}}
	next := a.next(contract.New(contract.MarkerInvalid, "x").WithDetails(map[string]any{"feature": "feat-x", "mode": "preserve", "commit": "s1"}))
	if len(next) < 3 || next[0].Command[4] != "squash" || next[1].Command[4] != "curated" ||
		!strings.Contains(next[len(next)-1].Why, "error.details.commit") {
		t.Fatalf("next = %+v", next)
	}
}

// apply <path> outside the plans directory is suggested by that path: apply
// <id> looks only in the plans directory. Another plan is still named by id.
func TestNextAppliesAPlanByItsPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const id, other = "20261009T120000Z-abcdef", "20261009T110000Z-123456"
	path, err := plan.Save(dir, plan.Plan{SchemaVersion: plan.SchemaVersion, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	a := testApp("apply", "apply", filepath.Base(path), "--json")
	a.env.Getwd = func() (string, error) { return dir, nil }
	next := a.next(contract.New(contract.ApprovalRequired, "x").WithDetails(map[string]any{"reason": "no_console", "plan_id": id}))
	if len(next) != 1 || !slices.Equal(next[0].Command, []string{"ghostbranch", "apply", path}) {
		t.Fatalf("next = %+v", next)
	}
	next = a.next(contract.New(contract.BranchExists, "x").WithDetails(map[string]any{"branch": "feat-x", "sha": "abc", "plan_id": other}))
	if len(next) == 0 || !slices.Equal(next[0].Command, []string{"ghostbranch", "apply", other}) {
		t.Fatalf("an earlier plan: next = %+v", next)
	}
}

// A merged feature whose record a newer Ghostbranch wrote isn't sync's to
// close: upgrade, or a person closes it. An unreadable record is closed by
// a person.
func TestNextForANewerOrUnreadableRecord(t *testing.T) {
	t.Parallel()
	a := testApp("status")
	a.warnings = []contract.Warning{{Code: contract.RecordNewer, Details: map[string]any{"feature": "feat-x", "record_version": "0.11"}}}
	next := a.featureNexts(nil, []engine.FeatureReport{
		{Name: "feat-x", State: feature.StateMerged, PRState: "merged"},
		{Name: "feat-y", State: feature.StateUnreadable},
	})
	if len(next) != 2 {
		t.Fatalf("next = %+v", next)
	}
	for i, name := range []string{"feat-x", "feat-y"} {
		if !slices.Equal(next[i].Command, []string{"ghostbranch", "feature", "close", name, "--repo", "acme/widget"}) || !next[i].NeedsHuman {
			t.Errorf("next[%d] = %+v", i, next[i])
		}
	}
	if !strings.Contains(next[0].Why, "newer Ghostbranch") || !strings.Contains(next[0].Why, "upgrade") {
		t.Errorf("newer why = %q", next[0].Why)
	}
	if want := "its record couldn't be read; run feature show feat-y for the error: run the command again if it was a passing failure, and if the record is corrupt a person closes it at the console"; next[1].Why != want {
		t.Errorf("unreadable why = %q, want %q", next[1].Why, want)
	}
}

// A sync whose close of a merged feature failed doesn't offer feature
// start: the feature is still there.
func TestSyncNextAfterAFailedClose(t *testing.T) {
	t.Parallel()
	offers := func(w contract.Warning) bool {
		a := testApp("sync", "sync")
		a.result = &engine.SyncResult{Surveyed: true}
		a.warnings = []contract.Warning{w}
		for _, n := range a.next(nil) {
			if slices.Contains(n.Command, "start") {
				return true
			}
		}
		return false
	}
	if offers(contract.Warning{Code: contract.FeatureNotClosed, Details: map[string]any{"feature": "feat-x", "error_code": "FORGE_ERROR"}}) {
		t.Error("feature start offered after a failed close")
	}
	if !offers(contract.Warning{Code: contract.FeatureNotClosed, Details: map[string]any{"feature": "feat-x", "error_code": "FORGE_ERROR",
		"deleted": map[string]string{"refs/heads/feat-x": "abc"}}}) {
		t.Error("the feature's refs are gone: feature start should be offered")
	}
}

// An error that names a job (a gate whose CI passed but whose result
// couldn't be written) offers wait on that job first: wait records it. A
// stale or missing job is dead, so wait on it isn't offered; INTERRUPTED
// and LOCKED keep their own steps.
func TestNextWaitsOnTheJobAnErrorNames(t *testing.T) {
	t.Parallel()
	const job = "gate/feat-x/0123456789ab"
	wait := []string{"ghostbranch", "wait", job, "--repo", "acme/widget"}
	a := testApp("gate", "gate", "feat-x", "--json")
	for _, code := range []contract.Code{contract.RefMoved, contract.Internal, contract.ForgeError} {
		next := a.next(contract.New(code, "x").WithDetails(map[string]any{"job": job}))
		if len(next) == 0 || !slices.Equal(next[0].Command, wait) ||
			next[0].Why != "the gate passed but its result wasn't recorded; wait records it" {
			t.Errorf("%s: next = %+v", code, next)
		}
	}
	for _, code := range []contract.Code{contract.JobStale, contract.JobNotFound} {
		for _, n := range a.next(contract.New(code, "x").WithDetails(map[string]any{"job": job, "current_job": ""})) {
			if slices.Equal(n.Command, wait) {
				t.Errorf("%s offers wait on the dead job: %+v", code, n)
			}
		}
	}
	const id = "20261009T120000Z-abcdef"
	next := a.next(contract.New(contract.Interrupted, "x").WithDetails(map[string]any{"job": job, "plan_id": id}))
	if len(next) != 1 || !slices.Equal(next[0].Command, []string{"ghostbranch", "apply", id}) {
		t.Errorf("interrupted after the plan: next = %+v", next)
	}
	next = a.next(contract.New(contract.Locked, "x").WithDetails(map[string]any{"job": job}))
	if len(next) != 1 || !slices.Equal(next[0].Command, wait) || !strings.Contains(next[0].Why, "held the lock") {
		t.Errorf("locked: next = %+v", next)
	}
}

// A shadow branch that moved under sync's push syncs again; any other push
// failure leaves only what the features need.
func TestNextForASyncPushFailure(t *testing.T) {
	t.Parallel()
	attention := []engine.FeatureReport{{Name: "feat-c", State: feature.StatePRClosed}}
	closeC := []string{"ghostbranch", "feature", "close", "feat-c", "--repo", "acme/widget"}
	a := testApp("sync", "sync")
	a.result = &engine.SyncResult{Surveyed: true, Attention: attention}
	next := a.next(contract.New(contract.RefMoved, "x"))
	if len(next) != 2 || !slices.Equal(next[0].Command, []string{"ghostbranch", "sync", "--repo", "acme/widget"}) ||
		next[0].Why != "a shadow branch moved during the push; sync again" ||
		!slices.Equal(next[1].Command, closeC) || !next[1].NeedsHuman {
		t.Fatalf("REF_MOVED: next = %+v", next)
	}
	next = a.next(contract.New(contract.PushRefused, "x"))
	if len(next) != 1 || !slices.Equal(next[0].Command, closeC) {
		t.Fatalf("PUSH_REFUSED: next = %+v", next)
	}
}

// An INTERRUPTED apply with nothing pending resumes where it stopped.
func TestNextForAnInterruptedApply(t *testing.T) {
	t.Parallel()
	a := testApp("apply", "apply", "20261009T120000Z-abcdef")
	next := a.next(contract.New(contract.Interrupted, "x"))
	if len(next) != 1 || !slices.Equal(next[0].Command, []string{"ghostbranch", "apply", "20261009T120000Z-abcdef"}) ||
		next[0].Why != "apply resumes where it stopped; run it again" {
		t.Fatalf("next = %+v", next)
	}
	next = testApp("sync", "sync").next(contract.New(contract.Interrupted, "x"))
	if len(next) != 1 || next[0].Why != "nothing is pending; run it again" {
		t.Fatalf("sync: next = %+v", next)
	}
}

// A person's replay names the pair, never a relative --repo: their
// terminal may not be in this checkout.
func TestAgainForAPersonReplacesARelativeRepo(t *testing.T) {
	t.Parallel()
	a := testApp("agents allow", "agents", "allow", "--repo", ".", "--non-interactive", "--json")
	if got := a.again(true); !slices.Equal(got, []string{"ghostbranch", "agents", "allow", "--repo", "acme/widget"}) {
		t.Fatalf("again(true) = %v", got)
	}
	if got := a.again(false); !slices.Equal(got, []string{"ghostbranch", "agents", "allow", "--repo", ".", "--non-interactive", "--json"}) {
		t.Fatalf("again(false) = %v", got)
	}
}

// A login GitHub refused offers `ghostbranch login` to a person, like the
// other device-flow failures.
func TestARefusedLoginOffersLogin(t *testing.T) {
	t.Parallel()
	a := testApp("sync", "sync", "--json")
	next := a.next(contract.New(contract.LoginUnavailable, "x").WithDetails(map[string]any{"reason": "refused"}))
	if len(next) != 1 || !slices.Equal(next[0].Command, []string{"ghostbranch", "login"}) || !next[0].NeedsHuman {
		t.Fatalf("next = %+v", next)
	}
}

// The stack errors and states each name the one step that moves the stack
// on.
func TestNextForStacks(t *testing.T) {
	t.Parallel()
	repo := []string{"--repo", "acme/widget"}
	cmd := func(args ...string) []string { return append(append([]string{"ghostbranch"}, args...), repo...) }
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		// A feature moved onto main that was rebased by hand past its old
		// fork point records where it sits now.
		{"fork point lost on main", contract.New(contract.ParentRewritten, "x").WithDetails(map[string]any{
			"feature": "feat-b", "parent": "main", "forked_at": "abc", "reason": "fork_point_lost"}),
			cmd("feature", "restack", "feat-b", "--onto", "main")},
		{"not a stack, stale gate", contract.New(contract.NotAStack, "x").WithDetails(map[string]any{
			"reason": "stale_gate", "feature": "feat-a", "features": []string{"feat-a", "feat-b"}}),
			cmd("gate", "feat-a")},
		{"not a stack, want", contract.New(contract.NotAStack, "x").WithDetails(map[string]any{
			"reason": "order", "feature": "feat-a", "features": []string{"feat-b", "feat-a"}, "want": []string{"feat-a", "feat-b"}}),
			cmd("promote", "feat-a", "feat-b", "--plan")},
		{"not a stack, parent merged", contract.New(contract.NotAStack, "x").WithDetails(map[string]any{
			"reason": "parent_merged", "feature": "feat-b", "parent": "feat-a"}),
			cmd("sync")},
		{"a stack plan went stale", contract.New(contract.PlanStale, "x").WithDetails(map[string]any{
			"feature": "feat-a", "public": "acme/widget", "mode": "squash", "moved": []string{"public_main"}, "features": []string{"feat-a", "feat-b"}}),
			[]string{"ghostbranch", "promote", "feat-a", "feat-b", "--plan", "--repo", "acme/widget"}},
	} {
		a := testApp("promote", "promote")
		if next := a.next(tc.err); len(next) != 1 || !slices.Equal(next[0].Command, tc.want) {
			t.Errorf("%s: next = %+v, want %v", tc.name, next, tc.want)
		}
	}
	a := testApp("status", "status")
	next := a.featureNexts(nil, []engine.FeatureReport{
		{Name: "feat-b", State: feature.StateParentStale, Parent: "feat-a"},
		{Name: "feat-c", State: feature.StateParentStale, Parent: "feat-a"},
		{Name: "feat-d", State: feature.StateParentStale, Parent: "main"},
		{Name: "feat-e", State: feature.StatePublicPRNeedsUpdate, Parent: "main"},
	})
	want := [][]string{
		cmd("feature", "restack", "feat-a"),
		cmd("feature", "restack", "feat-d", "--onto", "main"),
		cmd("feature", "restack", "feat-e", "--onto", "main"),
	}
	if len(next) != len(want) {
		t.Fatalf("featureNexts = %+v, want %v", next, want)
	}
	for i := range want {
		if !slices.Equal(next[i].Command, want[i]) {
			t.Errorf("featureNexts[%d] = %v, want %v", i, next[i].Command, want[i])
		}
	}
}

// A stale stack plan's re-plan can't carry the old plan's per-branch
// flags: its why says to pass them again.
func TestStackPlanStaleNextSaysToPassTheFlagsAgain(t *testing.T) {
	t.Parallel()
	a := testApp("apply", "apply")
	next := a.next(contract.New(contract.PlanStale, "x").WithDetails(map[string]any{
		"feature": "feat-a", "public": "acme/widget", "mode": "squash", "moved": []string{"public_main"}, "features": []string{"feat-a", "feat-b"}}))
	if len(next) != 1 || !strings.Contains(next[0].Why, "--mode, --mode-for and --title-for") {
		t.Fatalf("next = %+v", next)
	}
}

// Sync leaves a feature whose PR merged into the branch below it open; next
// hands its close to a person, who checks that branch first.
func TestNextForAFeatureMergedIntoItsParentsBranch(t *testing.T) {
	t.Parallel()
	a := testApp("sync", "sync")
	a.warnings = []contract.Warning{{Code: contract.FeatureNotClosed,
		Details: map[string]any{"feature": "feat-b", "reason": "merged_into_parent_branch", "base": "feat-a"}}}
	a.result = &engine.SyncResult{Surveyed: true, Attention: []engine.FeatureReport{{Name: "feat-b", State: feature.StateMerged, PRState: "merged"}}}
	next := a.next(nil)
	if len(next) != 1 || !slices.Equal(next[0].Command, []string{"ghostbranch", "feature", "close", "feat-b", "--repo", "acme/widget"}) ||
		!next[0].NeedsHuman || !strings.Contains(next[0].Why, "feat-a's public branch") {
		t.Fatalf("next = %+v", next)
	}
}

// The stack-path refusals of an update, PLAN_STALE after the PR moved, and
// a pr_behind feature each name the step that moves it on.
func TestNextForUpdates(t *testing.T) {
	t.Parallel()
	repo := []string{"--repo", "acme/widget"}
	cmd := func(args ...string) []string { return append(append([]string{"ghostbranch"}, args...), repo...) }
	a := testApp("promote", "promote", "feat-a")
	next := a.next(contract.New(contract.NotAStack, "x").WithDetails(map[string]any{
		"reason": "stale_gate", "feature": "feat-b", "features": []string{"feat-a", "feat-b", "feat-c"}}))
	if len(next) != 1 || !slices.Equal(next[0].Command, cmd("gate", "feat-b")) || !strings.Contains(next[0].Why, "feat-c") ||
		!strings.Contains(next[0].Why, "bottom up") {
		t.Errorf("stale_gate: next = %+v", next)
	}
	// A bottom gate built in another mode: gating again would loop, so next
	// promotes the bottom on its own in that mode; the branches above come
	// once that plan is applied, so they are named in why, not offered now.
	next = a.next(contract.New(contract.NotAStack, "x").WithDetails(map[string]any{
		"reason": "stale_gate", "feature": "feat-a", "features": []string{"feat-a", "feat-b"}, "update_mode": "squash"}))
	if len(next) != 1 || !slices.Equal(next[0].Command, cmd("promote", "feat-a", "--mode", "squash", "--plan")) ||
		!strings.Contains(next[0].Why, "once that plan is applied, promote feat-b --mode squash --plan") {
		t.Errorf("stale_gate update_mode: next = %+v", next)
	}
	next = a.next(contract.New(contract.NotAStack, "x").WithDetails(map[string]any{
		"reason": "open_above", "feature": "feat-a", "features": []string{"feat-a"}, "children": []string{"feat-b", "feat-c"}}))
	// Promoting either line would be refused naming the other: a person
	// closes or merges the other lines' PRs first, then promote feat-a.
	if len(next) != 2 || !slices.Equal(next[0].Command, cmd("status")) || !next[0].NeedsHuman ||
		!strings.Contains(next[0].Why, "feat-b, feat-c") || !slices.Equal(next[1].Command, cmd("promote", "feat-a", "--plan")) {
		t.Errorf("open_above: next = %+v", next)
	}
	next = a.next(contract.New(contract.NotAStack, "x").WithDetails(map[string]any{
		"reason": "parent_stale", "feature": "feat-b", "parent": "feat-a", "features": []string{"feat-a", "feat-b"}}))
	if len(next) != 2 || !slices.Equal(next[0].Command, cmd("feature", "restack", "feat-a")) {
		t.Errorf("parent_stale: next = %+v", next)
	}
	ap := testApp("apply", "apply")
	next = ap.next(contract.New(contract.PlanStale, "x").WithDetails(map[string]any{
		"feature": "feat-a", "public": "acme/widget", "mode": "squash", "moved": []string{"public_branch"}, "branch": "feat-a"}))
	if len(next) != 1 || !slices.Equal(next[0].Command, []string{"ghostbranch", "promote", "feat-a", "--mode", "squash", "--plan", "--repo", "acme/widget"}) {
		t.Errorf("PLAN_STALE public_branch: next = %+v", next)
	}
	job := "gate/feat-a/0123456789ab"
	st := testApp("status", "status")
	next = st.featureNexts(nil, []engine.FeatureReport{
		{Name: "feat-a", State: feature.StatePRBehind, Gate: &engine.GateSummary{Job: &job}},
		{Name: "feat-b", State: feature.StatePRBehind, Gate: &engine.GateSummary{Fresh: true, Checks: feature.ChecksPassed, Leak: feature.LeakClean}},
		{Name: "feat-c", State: feature.StatePRBehind},
		{Name: "feat-d", State: feature.StatePRBehind, PR: &engine.PRSync{Behind: 1, Foreign: 2}},
	})
	want := [][]string{cmd("wait", job), cmd("promote", "feat-b", "--plan"), cmd("gate", "feat-c"), cmd("feature", "pull", "feat-d")}
	if len(next) != len(want) {
		t.Fatalf("featureNexts = %+v, want %v", next, want)
	}
	for i := range want {
		if !slices.Equal(next[i].Command, want[i]) {
			t.Errorf("featureNexts[%d] = %v, want %v", i, next[i].Command, want[i])
		}
	}
}

// After a stack publish, next says merging the top PR merges them all.
func TestNextAfterAStackPublish(t *testing.T) {
	t.Parallel()
	a := testApp("apply", "apply")
	stack := 3
	a.result = &engine.ApplyResult{PublicPR: 7, StackNumber: &stack, PRs: []plan.StackPR{{Feature: "feat-a", Number: 6}, {Feature: "feat-b", Number: 7}}}
	next := a.next(nil)
	if len(next) != 1 || !slices.Equal(next[0].Command, []string{"ghostbranch", "sync", "--repo", "acme/widget"}) ||
		!strings.Contains(next[0].Why, "#7") || !strings.Contains(next[0].Why, "merges them all") {
		t.Fatalf("next = %+v", next)
	}
	// Plain chained PRs (no native stack) merge one by one, bottom first.
	a.result = &engine.ApplyResult{PublicPR: 7, PRs: []plan.StackPR{{Feature: "feat-a", Number: 6}, {Feature: "feat-b", Number: 7}}}
	if next = a.next(nil); len(next) != 1 || strings.Contains(next[0].Why, "merges them all") || !strings.Contains(next[0].Why, "#6 first") {
		t.Fatalf("plain chain: next = %+v", next)
	}
}

// F-29: next for a busy lock is the same command again, and says who holds
// it. A LOCKED with a job keeps its wait step.
func TestNextForLockedRunsTheCommandAgain(t *testing.T) {
	t.Parallel()
	a := testApp("sync", "sync", "--repo", "acme/widget")
	err := contract.New(contract.Locked, "busy").WithDetails(map[string]any{
		"held_by": map[string]any{"op": "gate", "pid": 13096, "since": "2026-10-13T07:58:14Z"}, "path": "/s/locks/acme__widget.lock"})
	next := a.next(err)
	if len(next) != 1 || !slices.Equal(next[0].Command, []string{"ghostbranch", "sync", "--repo", "acme/widget"}) || next[0].NeedsHuman ||
		!strings.Contains(next[0].Why, "(gate, pid 13096, since 2026-10-13T07:58:14Z)") || !strings.Contains(next[0].Why, "wait") {
		t.Fatalf("next = %+v", next)
	}
	err.Details["job"] = "gate/feat-x/0123456789ab"
	if next = a.next(err); len(next) != 1 || next[0].Command[1] != "wait" {
		t.Fatalf("with a job, next = %+v", next)
	}
}

// F-14 and review M6(b): next after NO_OPEN_PR for every reason.
func TestNextForNoOpenPRByReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		details map[string]any
		want    []string
		human   bool
	}{
		{"not published", map[string]any{"feature": "feat-x", "reason": "not_published"},
			[]string{"ghostbranch", "promote", "feat-x", "--plan", "--repo", "acme/widget"}, false},
		{"merged", map[string]any{"feature": "feat-x", "reason": "merged", "public_pr": 3},
			[]string{"ghostbranch", "sync", "--repo", "acme/widget"}, false},
		{"closed", map[string]any{"feature": "feat-x", "reason": "closed", "public_pr": 3},
			[]string{"ghostbranch", "feature", "show", "feat-x", "--repo", "acme/widget"}, false},
		{"branch gone", map[string]any{"feature": "feat-x", "reason": "branch_gone", "public_pr": 3},
			[]string{"ghostbranch", "feature", "show", "feat-x", "--repo", "acme/widget"}, false},
	} {
		a := testApp("feature pull", "feature", "pull", "feat-x", "--repo", "acme/widget")
		next := a.next(contract.New(contract.NoOpenPR, "x").WithField("feature").WithDetails(tc.details))
		if len(next) == 0 || !slices.Equal(next[0].Command, tc.want) || next[0].NeedsHuman != tc.human {
			t.Errorf("%s: next = %+v, want %v", tc.name, next, tc.want)
		}
	}
}

// Review M8: a holder whose text isn't as Ghostbranch writes it is left out
// of next's why.
func TestNextForLockedLeavesOutAnOddHolder(t *testing.T) {
	t.Parallel()
	a := testApp("sync", "sync", "--repo", "acme/widget")
	err := contract.New(contract.Locked, "busy").WithDetails(map[string]any{
		"held_by": map[string]any{"op": "gate\x1b]0;pwned\x07", "pid": 13096, "since": "2026-10-13T07:58:14Z"}})
	next := a.next(err)
	if len(next) != 1 || strings.Contains(next[0].Why, "pid") || strings.ContainsAny(next[0].Why, "\x1b\x07") {
		t.Fatalf("next = %+v", next)
	}
}

// Review M7: LOCKED on a command only a person can finish repeats it for a
// person: needs_human, without --json, naming the pair.
func TestNextForLockedOnAPersonOnlyCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		command string
		args    []string
		human   bool
	}{
		{"agents allow", []string{"agents", "allow", "--json"}, true},
		{"waivers add", []string{"waivers", "add", "feat-x", "f-0123456789ab", "--json"}, true},
		{"login", []string{"login", "--json"}, true},
		{"apply", []string{"apply", "20261005T120000Z-0a0b0c", "--json"}, true},
		{"apply", []string{"apply", "20261005T120000Z-0a0b0c", "--yes", "--json"}, false},
		{"sync", []string{"sync", "--json"}, false},
	} {
		a := testApp(tc.command, tc.args...)
		next := a.next(contract.New(contract.Locked, "busy"))
		if len(next) != 1 || next[0].NeedsHuman != tc.human || slices.Contains(next[0].Command, "--json") == tc.human {
			t.Errorf("%v: next = %+v", tc.args, next)
		}
	}
}

// F-12: stale_gate says the plan updates the stack's PRs only when they are
// open (details.updating); otherwise it says to promote the stack again.
func TestStaleGateSaysThePlanUpdatesOnlyWhenPRsAreOpen(t *testing.T) {
	t.Parallel()
	a := testApp("promote", "promote", "feat-a", "feat-b", "--plan")
	ce := contract.New(contract.NotAStack, "stale").WithDetails(map[string]any{
		"reason": "stale_gate", "feature": "feat-a", "features": []string{"feat-a", "feat-b"}})
	if n := a.next(ce); len(n) != 1 || !strings.Contains(n[0].Why, "then promote the stack again") || strings.Contains(n[0].Why, "updates them all") {
		t.Fatalf("no open PRs: %+v", n)
	}
	ce.Details["updating"] = true
	if n := a.next(ce); len(n) != 1 || !strings.Contains(n[0].Why, "the plan updates them all") {
		t.Fatalf("open PRs: %+v", n)
	}
}
