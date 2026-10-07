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
		c[contract.RepoGuard] != 0 || c[contract.PushTriggersOnSync] != 0 || c[contract.OverlayOutdated] != 0 {
		t.Fatalf("warnings = %+v", warns)
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
