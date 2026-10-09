package engine_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// startFeature starts a feature and adds one developer commit to it in the
// shadow repo, the way a developer's push would. It returns the new head.
func (h *harness) startFeature(name string, files map[string]string) string {
	h.t.Helper()
	if _, _, err := h.eng.FeatureStart(h.ctx, h.pair, name); err != nil {
		h.t.Fatalf("feature start %s: %v", name, err)
	}
	return testutil.Commit(h.t, h.shadowGit(), name, "work on "+name, files)
}

func TestFeatureStartCreatesBranchAndMetadata(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	res, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-x")
	if err != nil || res.Branch != "feat-x" || !reflect.DeepEqual(res.Checkout, [][]string{{"git", "fetch", "shadow"}, {"git", "switch", "feat-x"}}) ||
		res.CheckoutHint != "git fetch shadow && git switch feat-x" {
		t.Fatalf("start: %+v %v", res, err)
	}
	sh := h.shadowGit()
	head, _ := sh.MustResolve(h.ctx, "refs/heads/feat-x")
	main, _ := sh.MustResolve(h.ctx, "refs/heads/main")
	if head != main || res.Base != main {
		t.Fatalf("feature must start at shadow main: %s vs %s", head, main)
	}
	raw, ok, _ := sh.ShowFile(h.ctx, "refs/ghostbranch/features/feat-x", feature.MetaFile)
	m, err := feature.Decode(raw)
	if !ok || err != nil || m.Name != "feat-x" || m.CreatedBy != "human:octo" || m.Base != main || m.Gate != nil {
		t.Fatalf("meta = %+v %v", m, err)
	}
	_, _, err = h.eng.FeatureStart(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.BranchExists)
}

// Review Focus 1: ref path conflicts are reported before git hits them.
func TestFeatureStartRefPathConflicts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("auth", map[string]string{"a.txt": "a\n"})
	for _, name := range []string{"auth/login", "auth"} {
		_, _, err := h.eng.FeatureStart(h.ctx, h.pair, name)
		ce := wantCode(t, err, contract.BranchExists)
		if ce.Field != "name" || ce.Details["conflicts_with"] != "auth" {
			t.Fatalf("%s: %+v", name, ce)
		}
	}
	h.startFeature("pay/v2", map[string]string{"p.txt": "p\n"})
	_, _, err := h.eng.FeatureStart(h.ctx, h.pair, "pay")
	if ce := wantCode(t, err, contract.BranchExists); ce.Details["conflicts_with"] != "pay/v2" {
		t.Fatalf("pay: %+v", ce)
	}
}

func TestFeatureStartRejectsPublicBranchNamesAndBadNames(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	testutil.Commit(t, h.pubGit, "release", "r", map[string]string{"r.txt": "r\n"})
	_, _, err := h.eng.FeatureStart(h.ctx, h.pair, "release")
	if ce := wantCode(t, err, contract.BranchExists); ce.Details["in"] != "public" {
		t.Fatalf("public collision: %+v", ce)
	}
	_, _, err = h.eng.FeatureStart(h.ctx, h.pair, "ghostbranch/gate/x")
	_ = wantCode(t, err, contract.NameInvalid)
}

func TestFeatureStartPercentInExistingBranchName(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	testutil.Commit(t, h.pubGit, "pct/a%sb", "p", map[string]string{"p.txt": "p\n"})
	_, _, err := h.eng.FeatureStart(h.ctx, h.pair, "pct")
	ce := wantCode(t, err, contract.BranchExists)
	if !strings.Contains(ce.Message, "pct/a%sb") || strings.Contains(ce.Message, "%!") {
		t.Fatalf("message garbled: %q", ce.Message)
	}
}

// installShadowHook makes the fake forge's shadow repo refuse every push with
// msg, the way a ruleset, a missing token scope or a server hook does.
func (h *harness) installShadowHook(msg string) {
	h.t.Helper()
	hooks := filepath.Join(h.forge.RepoDir(shadowName), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		h.t.Fatal(err)
	}
	script := "#!/bin/sh\necho '" + msg + "' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-receive"), []byte(script), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func TestPushTheServerRefusesIsPushRefusedNotBranchExists(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.installShadowHook("GH013: repository rule violations found")
	_, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.PushRefused)
	if ce.Details["remote"] != "shadow" || ce.Details["reason"] == nil || !strings.Contains(ce.Details["reason"].(string), "pre-receive hook declined") {
		t.Fatalf("details = %v", ce.Details)
	}
	refs, _ := ce.Details["refs"].([]string)
	if len(refs) == 0 {
		t.Fatalf("details.refs = %v", ce.Details["refs"])
	}
	if !strings.HasPrefix(ce.Message, "the shadow repo refused the push: ") || strings.Contains(ce.Message, "Workflows write") {
		t.Fatalf("message = %q", ce.Message)
	}
}

func TestPushRefusedForWorkflowScopeSaysWhichPermission(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.installShadowHook("refusing to allow an OAuth App to create or update workflow .github/workflows/x.yml without workflow scope")
	_, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.PushRefused)
	if !strings.Contains(ce.Message, "without workflow scope") || !strings.HasSuffix(ce.Message, "; the token needs Workflows write (fine-grained) or the workflow scope (gh/OAuth)") {
		t.Fatalf("message = %q", ce.Message)
	}
}

// A refusal that only mentions workflows (a ruleset's required workflows)
// is not a missing token permission: no Workflows-write hint.
func TestPushRefusedForRequiredWorkflowsGivesNoPermissionHint(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.installShadowHook("GH013: Repository rule violations found: Required workflows must pass")
	_, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.PushRefused)
	if !strings.Contains(ce.Message, "Required workflows") || strings.Contains(ce.Message, "Workflows write") {
		t.Fatalf("message = %q", ce.Message)
	}
}

// Shadow main behind public main: feature start refuses with SHADOW_BEHIND
// (exit 4) before it creates anything, and --plan refuses the same way.
func TestFeatureStartRefusesWhenShadowMainIsBehind(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	sm, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/main")
	pm := testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "1\n"})
	pushes, refs := len(h.pushes), h.shadowRefs()
	for _, dry := range []bool{true, false} {
		h.eng.DryRun = dry
		res, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-x")
		h.eng.DryRun = false
		ce := wantCode(t, err, contract.ShadowBehindError)
		if res != nil || ce.Code.Exit() != contract.ExitConflict || ce.Details["public_main"] != pm || ce.Details["shadow_main"] != sm ||
			!strings.Contains(ce.Message, "ghostbranch sync") {
			t.Fatalf("dry=%v: %+v %+v", dry, res, ce)
		}
	}
	if len(h.pushes) != pushes || !reflect.DeepEqual(h.shadowRefs(), refs) {
		t.Fatal("a refused feature start created something")
	}
	h.syncOK(engine.SyncOptions{})
	if res, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-x"); err != nil || res.Base != pm {
		t.Fatalf("start after sync: %+v %v", res, err)
	}
}

// lockMirrorRef makes git refuse to change ref in the mirror, as a stale
// lock file does.
func (h *harness) lockMirrorRef(ref string) {
	h.t.Helper()
	path := filepath.Join(h.eng.WS.MirrorDir(h.pair), filepath.FromSlash(ref)+".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// warningsFor is the warnings with code.
func warningsFor(warns []contract.Warning, code contract.WarningCode) []contract.Warning {
	var out []contract.Warning
	for _, w := range warns {
		if w.Code == code {
			out = append(out, w)
		}
	}
	return out
}

// The push succeeded but the mirror couldn't follow: MIRROR_NOT_UPDATED,
// and feature start still succeeds.
func TestFeatureStartWarnsWhenTheMirrorCantBeUpdated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	meta := "refs/shadow-meta/features/feat-x"
	h.lockMirrorRef(meta)
	res, warns, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-x")
	if err != nil || res == nil {
		t.Fatalf("start = %+v %v", res, err)
	}
	ws := warningsFor(warns, contract.MirrorNotUpdated)
	if len(ws) != 1 || ws[0].Details["ref"] != meta || !strings.Contains(ws[0].Message, meta) {
		t.Fatalf("warnings = %+v", warns)
	}
	if _, ok := h.shadowRefs()["refs/ghostbranch/features/feat-x"]; !ok {
		t.Fatal("the shadow repo lacks the metadata")
	}
}
