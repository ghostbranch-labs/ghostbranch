package engine_test

import (
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/managed"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

const guardedCI = `name: ci
on:
  push:
    branches: [main]
  pull_request:
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ secrets.NPM_TOKEN }}
  publish:
    if: github.repository == 'acme/widget'
    runs-on: ubuntu-latest
    steps:
      - id: odd
        if: startsWith(github.repository, 'acme/widget')
        run: echo ${{ secrets.DEPLOY_KEY }}
`

func codes(ws []contract.Warning) map[contract.WarningCode]int {
	out := map[contract.WarningCode]int{}
	for _, w := range ws {
		out[w.Code]++
	}
	return out
}

func TestAuditReportsGuardsSecretsAndMinutes(t *testing.T) {
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "main", "guarded ci", map[string]string{".github/workflows/ci.yml": guardedCI})
	h.init()
	h.forge.Secrets = map[string][]string{shadowName: {"NPM_TOKEN"}}
	res, warns, err := h.eng.Audit(h.ctx, h.pair)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Guards) != 2 || !res.Guards[0].Rewritable || res.Guards[1].Rewritable || res.GuardMode != "rewrite" {
		t.Fatalf("guards = %+v", res.Guards)
	}
	if !res.Secrets.Listed || strings.Join(res.Secrets.Missing, ",") != "DEPLOY_KEY" {
		t.Fatalf("secrets = %+v", res.Secrets)
	}
	if !res.Minutes.Billed || res.Minutes.ScheduledSync != nil {
		t.Fatalf("minutes = %+v", res.Minutes)
	}
	if len(res.Overlay) != 1 || res.Overlay[0].Status != managed.Current {
		t.Fatalf("overlay = %+v", res.Overlay)
	}
	if len(res.PushTriggers) != 1 {
		t.Fatalf("push triggers = %v", res.PushTriggers)
	}
	c := codes(warns)
	if c[contract.RepoGuardUnrewritable] != 1 || c[contract.SecretsMissing] != 1 || c[contract.ActionsMinutesBilled] != 1 ||
		c[contract.RepoGuard] != 0 || c[contract.PushTriggersOnSync] != 1 || c[contract.OverlayOutdated] != 0 {
		t.Fatalf("warnings = %+v", warns)
	}
}

// ghostbranch sync pushes with the person's token, so push-triggered
// workflows start in the shadow repo even with scheduled sync off.
func TestAuditWarnsAboutPushTriggersWithScheduleOff(t *testing.T) {
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "main", "guarded ci", map[string]string{".github/workflows/ci.yml": guardedCI})
	h.init()
	_, warns, err := h.eng.Audit(h.ctx, h.pair)
	if err != nil {
		t.Fatal(err)
	}
	var w *contract.Warning
	for i := range warns {
		if warns[i].Code == contract.PushTriggersOnSync {
			w = &warns[i]
		}
	}
	guard := "github.event_name != 'push' || github.repository == 'acme/widget'"
	if w == nil || w.Details["guard"] != guard || !strings.Contains(w.Message, "if: "+guard) ||
		!strings.Contains(w.Message, ".github/workflows/ci.yml") || !strings.Contains(w.Message, "Actions write") {
		t.Fatalf("warnings = %+v", warns)
	}
	if paths, _ := w.Details["paths"].([]string); len(paths) != 1 || paths[0] != ".github/workflows/ci.yml" {
		t.Fatalf("details = %+v", w.Details)
	}
}

func TestAuditWithoutSecretsPermission(t *testing.T) {
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "main", "guarded ci", map[string]string{".github/workflows/ci.yml": guardedCI})
	h.init()
	h.forge.SecretsForbidden = map[string]bool{shadowName: true}
	res, warns, err := h.eng.Audit(h.ctx, h.pair)
	if err != nil || res.Secrets.Listed || len(res.Secrets.Missing) != 0 || len(res.Secrets.Referenced) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if codes(warns)[contract.SecretsUnlisted] != 1 {
		t.Fatalf("warnings = %+v", warns)
	}
}

func TestAuditWarnModeAndScheduledSync(t *testing.T) {
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "main", "guarded ci", map[string]string{".github/workflows/ci.yml": guardedCI})
	h.init()
	h.setConfig("rewrite_repo_guards: rewrite", "rewrite_repo_guards: warn")
	h.setConfig(`schedule: "off"`, `schedule: "23 */6 * * *"`) // wanted, not installed: missing
	res, warns, err := h.eng.Audit(h.ctx, h.pair)
	if err != nil {
		t.Fatal(err)
	}
	c := codes(warns)
	if c[contract.RepoGuard] != 1 || c[contract.RepoGuardUnrewritable] != 1 || c[contract.OverlayOutdated] != 1 {
		t.Fatalf("warnings = %+v", warns)
	}
	if res.GuardMode != "warn" {
		t.Fatalf("guard mode = %q", res.GuardMode)
	}
	if res.Overlay[0].Status != managed.Missing || res.Minutes.ScheduledSync == nil {
		t.Fatalf("%+v %+v", res.Overlay, res.Minutes)
	}
}

// setConfig replaces old with new in the overlay's .ghostbranch.yml.
func (h *harness) setConfig(old, new string) {
	h.t.Helper()
	raw, ok, err := h.shadowGit().ShowFile(h.ctx, "refs/heads/"+engine.OverlayBranch, ".ghostbranch.yml")
	if err != nil || !ok {
		h.t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(raw), old) {
		h.t.Fatalf("config has no %q", old)
	}
	testutil.Commit(h.t, h.shadowGit(), engine.OverlayBranch, "config",
		map[string]string{".ghostbranch.yml": strings.Replace(string(raw), old, new, 1)})
}

// The advised guard names the public repo as GitHub spells it, not as the
// pair was typed.
func TestAuditPushGuardUsesTheCanonicalName(t *testing.T) {
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "main", "guarded ci", map[string]string{".github/workflows/ci.yml": guardedCI})
	h.init()
	h.forge.SetFullName(publicName, "Acme/Widget")
	_, warns, err := h.eng.Audit(h.ctx, h.pair)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range warns {
		if w.Code == contract.PushTriggersOnSync {
			if want := "github.event_name != 'push' || github.repository == 'Acme/Widget'"; w.Details["guard"] != want {
				t.Fatalf("guard = %v, want %q", w.Details["guard"], want)
			}
			return
		}
	}
	t.Fatalf("no PUSH_TRIGGERS_ON_SYNC in %+v", warns)
}

// Following the audit's advice clears the warning: a file whose every job
// carries the push guard is no longer a push trigger, and a job that already
// had an if: keeps it, combined with the guard.
func TestAuditPushTriggersClearOnceJobsCarryTheGuard(t *testing.T) {
	h := newHarness(t)
	const guarded = "on:\n  push:\n    branches: [main]\n  pull_request:\njobs:\n" +
		"  test:\n    if: github.event_name != 'push' || github.repository == 'acme/widget'\n    runs-on: x\n    steps:\n      - run: echo\n" +
		"  lint:\n    if: matrix.os == 'linux' && (github.event_name != 'push' || github.repository == 'acme/widget')\n    runs-on: x\n    steps:\n      - run: echo\n"
	testutil.Commit(t, h.pubGit, "main", "push-guarded ci", map[string]string{".github/workflows/ci.yml": guarded})
	h.init()
	res, warns, err := h.eng.Audit(h.ctx, h.pair)
	if err != nil {
		t.Fatal(err)
	}
	c := codes(warns)
	if len(res.PushTriggers) != 0 || c[contract.PushTriggersOnSync] != 0 || len(res.Guards) != 0 || c[contract.RepoGuard] != 0 {
		t.Fatalf("push triggers = %v guards = %+v warnings = %+v", res.PushTriggers, res.Guards, warns)
	}
}

func TestAuditPushGuardAdviceCombinesWithAnExistingIf(t *testing.T) {
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "main", "guarded ci", map[string]string{".github/workflows/ci.yml": guardedCI})
	h.init()
	_, warns, err := h.eng.Audit(h.ctx, h.pair)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range warns {
		if w.Code == contract.PushTriggersOnSync {
			if !strings.Contains(w.Message, "<existing> && (github.event_name != 'push' || github.repository == 'acme/widget')") {
				t.Fatalf("message = %s", w.Message)
			}
			return
		}
	}
	t.Fatalf("no PUSH_TRIGGERS_ON_SYNC in %+v", warns)
}
