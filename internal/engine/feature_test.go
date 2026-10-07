package engine_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
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
	h := newHarness(t)
	h.init()
	res, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-x")
	if err != nil || res.Branch != "feat-x" || res.Checkout != "git fetch && git switch feat-x" {
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
	h := newHarness(t)
	h.init()
	h.installShadowHook("GH013: Repository rule violations found: Required workflows must pass")
	_, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.PushRefused)
	if !strings.Contains(ce.Message, "Required workflows") || strings.Contains(ce.Message, "Workflows write") {
		t.Fatalf("message = %q", ce.Message)
	}
}
