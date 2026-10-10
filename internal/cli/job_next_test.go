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
	a := &app{env: Env{Getenv: func(string) string { return "" }}}
	if next := a.next(contract.New(contract.Locked, "x")); len(next) != 0 {
		t.Errorf("a plain LOCKED offers nothing new: %+v", next)
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
