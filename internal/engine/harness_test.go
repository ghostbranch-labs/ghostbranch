package engine_test

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge/fake"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

const (
	publicName = "acme/widget"
	shadowName = "acme/widget-shadow"
)

type pushEvent struct {
	op, remote string
	refspecs   []string
}

type harness struct {
	t      *testing.T
	ctx    context.Context
	forge  *fake.Forge
	eng    *engine.Engine
	clock  *testutil.Clock
	pair   workspace.Pair
	pubGit *gitx.Git
	pushes []pushEvent
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	f := fake.New(filepath.Join(root, "forge"))
	dir, err := f.AddRepo(publicName, false)
	if err != nil {
		t.Fatal(err)
	}
	pubGit := gitx.New(dir)
	testutil.Commit(t, pubGit, "main", "initial", map[string]string{
		"README.md":                "# widget\n",
		".github/workflows/ci.yml": "name: ci\non: [pull_request]\n",
	})
	ws, err := workspace.Open(filepath.Join(root, "home"), "default")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ctx: context.Background(), forge: f, pubGit: pubGit,
		clock: testutil.NewClock(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)),
		pair:  workspace.Pair{Public: publicName, Shadow: shadowName}}
	h.eng = &engine.Engine{
		Forge: f, WS: ws, Actor: "human:octo",
		Now: h.clock.Now, Sleep: h.clock.Sleep, Rand: &testutil.SeqReader{}, Progress: io.Discard,
		NewScanner:   func(s overlay.Set) (leak.Scanner, error) { return leak.NewGitleaks(s) },
		PollInterval: time.Second, SettleWindow: 5 * time.Second,
		OnPush: func(op, remote string, specs []string) {
			h.pushes = append(h.pushes, pushEvent{op, remote, specs})
		},
	}
	return h
}

func (h *harness) init() {
	h.t.Helper()
	if _, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName}); err != nil {
		h.t.Fatalf("init: %v", err)
	}
}

func (h *harness) shadowGit() *gitx.Git { return gitx.New(h.forge.RepoDir(shadowName)) }

func (h *harness) publicRefs() map[string]string { //nolint:unused // used by later tests
	h.t.Helper()
	refs, err := h.pubGit.Refs(h.ctx, "refs")
	if err != nil {
		h.t.Fatal(err)
	}
	return refs
}

func (h *harness) ledger() []ledger.Entry {
	h.t.Helper()
	es, err := ledger.Read(h.eng.WS.LedgerPath())
	if err != nil {
		h.t.Fatal(err)
	}
	return es
}

func codeOf(err error) contract.Code {
	if err == nil {
		return ""
	}
	return contract.From(err).Code
}

func wantCode(t *testing.T, err error, want contract.Code) *contract.Error {
	t.Helper()
	if got := codeOf(err); got != want {
		t.Fatalf("want %s, got %v", want, err)
	}
	return contract.From(err)
}
