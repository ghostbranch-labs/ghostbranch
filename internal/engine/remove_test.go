package engine_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/waiver"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

const otherPublic, otherShadow = "acme/other", "acme/other-shadow"

// removeFixture is an initialized pair beside a second registered pair,
// each with one grant, one waiver and one plan file.
func removeFixture(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.init()
	ws := h.eng.WS
	ws.AddPair(workspace.Pair{Public: otherPublic, Shadow: otherShadow})
	if err := ws.Save(); err != nil {
		t.Fatal(err)
	}
	now := h.clock.Now()
	h.grant(6 * time.Hour)
	exp := now.Add(time.Hour)
	if _, err := h.eng.Grants.Put(grant.Grant{ID: "g-000002", Public: otherPublic, Shadow: otherShadow,
		CreatedAt: now, ExpiresAt: &exp, CreatedBy: "human:octo"}, now); err != nil {
		t.Fatal(err)
	}
	if err := h.eng.Waivers.Add([]waiver.Waiver{
		{ID: "f-000000000001", Public: publicName, Rule: "r", Where: "file", Path: "a", MatchSHA256: "x", Reason: "ok", CreatedAt: now, CreatedBy: "human:octo"},
		{ID: "f-000000000002", Public: otherPublic, Rule: "r", Where: "file", Path: "b", MatchSHA256: "y", Reason: "ok", CreatedAt: now, CreatedBy: "human:octo"},
	}, now); err != nil {
		t.Fatal(err)
	}
	for id, pub := range map[string]string{"20261005T120000Z-00000a": publicName, "20261005T120000Z-00000b": otherPublic} {
		if _, err := plan.Save(ws.PlansDir(), plan.Plan{SchemaVersion: plan.SchemaVersion, ID: id, Pair: plan.Pair{Public: pub, Shadow: pub + "-shadow"}}); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

type removeState struct {
	pairs   []string
	dir     bool
	grants  []string
	waivers []string
	plans   []string
}

func stateOf(t *testing.T, h *harness) removeState {
	t.Helper()
	var s removeState
	ws, err := workspace.Open(filepath.Dir(filepath.Dir(h.eng.WS.Dir)), h.eng.WS.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ws.Pairs {
		s.pairs = append(s.pairs, p.Public)
	}
	_, err = os.Stat(h.eng.WS.PairDir(h.pair))
	s.dir = err == nil
	gs, err := h.eng.Grants.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gs {
		s.grants = append(s.grants, g.Public)
	}
	ws2, err := h.eng.Waivers.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws2 {
		s.waivers = append(s.waivers, w.Public)
	}
	entries, err := os.ReadDir(h.eng.WS.PlansDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	for _, e := range entries {
		s.plans = append(s.plans, e.Name())
	}
	sort.Strings(s.pairs)
	sort.Strings(s.grants)
	sort.Strings(s.waivers)
	return s
}

func TestRemoveIsLocalAndNeedsAConsole(t *testing.T) {
	t.Parallel()
	h := removeFixture(t)
	before := stateOf(t, h)
	if !before.dir || len(before.pairs) != 2 || len(before.grants) != 2 || len(before.waivers) != 2 || len(before.plans) != 2 {
		t.Fatalf("fixture: %+v", before)
	}
	pushes, refs := len(h.pushes), h.publicRefs()

	// No one at a console: refused, and nothing changes.
	h.eng.Approver = nil
	_, _, err := h.eng.Remove(h.ctx, h.pair)
	ce := wantCode(t, err, contract.ApprovalRequired)
	if ce.Details["reason"] != "no_console" {
		t.Fatalf("details: %v", ce.Details)
	}
	if got := stateOf(t, h); !equalState(got, before) {
		t.Fatalf("a refused remove changed state:\n got %+v\nwant %+v", got, before)
	}

	// At a console. A nil forge panics on any call: remove is local.
	h.eng.Approver = h.approver
	h.eng.Forge = nil
	res, _, err := h.eng.Remove(h.ctx, h.pair)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.approver.asked) != 1 || !strings.Contains(h.approver.asked[0], publicName) || !strings.Contains(h.approver.asked[0], shadowName) {
		t.Fatalf("the console was asked %q", h.approver.asked)
	}
	wantPlan := filepath.Join(h.eng.WS.PlansDir(), "20261005T120000Z-00000a.json")
	if res.Public != publicName || res.Shadow != shadowName || res.Grants != 1 || res.Waivers != 1 ||
		len(res.Plans) != 1 || res.Plans[0] != wantPlan || res.Dir != h.eng.WS.PairDir(h.pair) || res.Preview != nil {
		t.Fatalf("result: %+v", res)
	}
	after := stateOf(t, h)
	want := removeState{pairs: []string{otherPublic}, grants: []string{otherPublic}, waivers: []string{otherPublic},
		plans: []string{"20261005T120000Z-00000b.json"}}
	if !equalState(after, want) {
		t.Fatalf("after remove:\n got %+v\nwant %+v", after, want)
	}
	if _, ok := h.eng.WS.Find(publicName); ok {
		t.Fatal("the in-memory workspace still has the pair")
	}
	if len(h.pushes) != pushes {
		t.Fatalf("remove pushed: %+v", h.pushes[pushes:])
	}
	h.eng.Forge = h.forge
	if !equalRefs(h.publicRefs(), refs) {
		t.Fatal("remove changed the public repo")
	}
	es := h.ledger()
	last := es[len(es)-1]
	if last.Op != "remove" || last.Pair != publicName || last.ExitCode != 0 || last.Summary == "" {
		t.Fatalf("ledger: %+v", last)
	}
	// The ledger keeps its history: init's entry is still there.
	if es[0].Op != "init" {
		t.Fatalf("ledger lost its history: %+v", es[0])
	}
}

// A workspace.json entry whose folder would not sit inside pairs/ is
// refused before the console, the lock or any change: RemoveAll must never
// reach the workspace itself or beyond.
func TestRemoveRefusesAPairFolderOutsidePairs(t *testing.T) {
	t.Parallel()
	for _, public := range []string{"..", `a\..\..`, `a/..\..`, "a/..", "../.."} {
		t.Run(public, func(t *testing.T) {
			t.Parallel()
			h := removeFixture(t)
			bad := workspace.Pair{Public: public, Shadow: "acme/bad-shadow"}
			h.eng.WS.Pairs = append(h.eng.WS.Pairs, bad)
			if err := h.eng.WS.Save(); err != nil {
				t.Fatal(err)
			}
			before := stateOf(t, h)
			for _, dry := range []bool{true, false} {
				h.eng.DryRun = dry
				_, _, err := h.eng.Remove(h.ctx, bad)
				wantCode(t, err, contract.Internal)
				if len(h.approver.asked) != 0 {
					t.Fatalf("the console was asked %q", h.approver.asked)
				}
				if got := stateOf(t, h); !equalState(got, before) {
					t.Fatalf("a refused remove changed state:\n got %+v\nwant %+v", got, before)
				}
				if _, err := os.Stat(filepath.Join(h.eng.WS.Dir, "workspace.json")); err != nil {
					t.Fatalf("the workspace is gone: %v", err)
				}
				if _, ok := h.eng.WS.Find(public); !ok {
					t.Fatal("the bad entry was dropped")
				}
			}
		})
	}
}

func TestRemovePlanChangesNothing(t *testing.T) {
	t.Parallel()
	h := removeFixture(t)
	before := stateOf(t, h)
	entries := len(h.ledger())
	h.eng.DryRun = true
	h.eng.Approver = nil // a preview needs no console
	res, _, err := h.eng.Remove(h.ctx, h.pair)
	if err != nil {
		t.Fatal(err)
	}
	if res.Preview == nil || res.Grants != 1 || res.Waivers != 1 || len(res.Plans) != 1 {
		t.Fatalf("result: %+v", res)
	}
	var lines []string
	for _, op := range res.Preview.Ops {
		switch op["op"] {
		case "remove_file", "write_file":
			lines = append(lines, op["op"].(string)+" "+op["path"].(string))
		default:
			lines = append(lines, op["op"].(string)+" "+op["public"].(string))
		}
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		"revoke " + publicName,
		"unwaive " + publicName,
		"remove_file " + filepath.Join(h.eng.WS.PlansDir(), "20261005T120000Z-00000a.json"),
		"remove_file " + h.eng.WS.PairDir(h.pair),
		"write_file " + filepath.Join(h.eng.WS.Dir, "workspace.json"),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("preview lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, otherPublic) || strings.Contains(got, "00000b") {
		t.Errorf("preview touches the other pair:\n%s", got)
	}
	if after := stateOf(t, h); !equalState(after, before) {
		t.Fatalf("--plan changed state:\n got %+v\nwant %+v", after, before)
	}
	if n := len(h.ledger()); n != entries {
		t.Fatalf("--plan wrote %d ledger entries", n-entries)
	}
}

func equalState(a, b removeState) bool {
	return strings.Join(a.pairs, ",") == strings.Join(b.pairs, ",") && a.dir == b.dir &&
		strings.Join(a.grants, ",") == strings.Join(b.grants, ",") && strings.Join(a.waivers, ",") == strings.Join(b.waivers, ",") &&
		strings.Join(a.plans, ",") == strings.Join(b.plans, ",")
}

func equalRefs(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestLog(t *testing.T) {
	t.Parallel()
	ws, err := workspace.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatal(err)
	}
	// A missing ledger is an empty list, never null.
	res, err := engine.Log(ws, engine.LogOptions{})
	if err != nil || res.Entries == nil || len(res.Entries) != 0 {
		t.Fatalf("missing ledger: %+v %v", res, err)
	}
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	add := func(i int, actor, op, feat string) {
		t.Helper()
		if err := ledger.Append(ws.LedgerPath(), ledger.Entry{TS: t0.Add(time.Duration(i) * time.Hour), Actor: actor, Op: op,
			Pair: "acme/widget", Feature: feat, Summary: op + " " + feat}); err != nil {
			t.Fatal(err)
		}
	}
	add(0, "human:octo", "init", "")
	add(1, "agent:claude-code", "feature start", "feat-x")
	add(2, "agent:claude-code", "gate", "feat-x")
	add(3, "human:octo", "promote", "feat-x")
	add(4, "agent:codex", "gate", "feat-y")
	if err := ledger.Append(ws.LedgerPath(), ledger.Entry{TS: t0.Add(4*time.Hour + time.Minute), Actor: "human:octo", Op: "remove",
		Pair: "acme/other", Summary: "remove"}); err != nil {
		t.Fatal(err)
	}
	ops := func(r *engine.LogResult) string {
		var out []string
		for _, e := range r.Entries {
			out = append(out, e.Op+"/"+e.Feature)
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		name string
		o    engine.LogOptions
		want string
	}{
		{"newest first", engine.LogOptions{}, "remove/,gate/feat-y,promote/feat-x,gate/feat-x,feature start/feat-x,init/"},
		{"pair", engine.LogOptions{Pair: "acme/other"}, "remove/"},
		{"pair ignores case", engine.LogOptions{Pair: "ACME/Widget", Op: "gate"}, "gate/feat-y,gate/feat-x"},
		{"feature", engine.LogOptions{Feature: "feat-x"}, "promote/feat-x,gate/feat-x,feature start/feat-x"},
		{"op is exact", engine.LogOptions{Op: "gate"}, "gate/feat-y,gate/feat-x"},
		{"op is not a prefix", engine.LogOptions{Op: "gat"}, ""},
		{"actor prefix", engine.LogOptions{ActorPrefix: "agent:"}, "gate/feat-y,gate/feat-x,feature start/feat-x"},
		{"actor whole", engine.LogOptions{ActorPrefix: "human:octo"}, "remove/,promote/feat-x,init/"},
		{"since", engine.LogOptions{Since: t0.Add(3 * time.Hour)}, "remove/,gate/feat-y,promote/feat-x"},
		{"limit", engine.LogOptions{Limit: 2}, "remove/,gate/feat-y"},
	} {
		res, err := engine.Log(ws, c.o)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := ops(res); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if res.Entries == nil {
			t.Errorf("%s: entries must never be null", c.name)
		}
	}
	for i := 5; i < 65; i++ {
		add(i, "app", "sync", "")
	}
	res, err = engine.Log(ws, engine.LogOptions{})
	if err != nil || len(res.Entries) != 50 || !res.Entries[0].TS.Equal(t0.Add(64*time.Hour)) {
		t.Fatalf("default limit: %d entries, first %v, %v", len(res.Entries), res.Entries[0].TS, err)
	}
}

func TestParseSince(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"24h":                  now.Add(-24 * time.Hour),
		"90m":                  now.Add(-90 * time.Minute),
		"2026-10-01T00:00:00Z": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		"":                     {},
	} {
		got, err := engine.ParseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: got %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"yesterday", "-1h", "0s", "2026-10-01"} {
		_, err := engine.ParseSince(bad, now)
		if ce := contract.From(err); ce == nil || ce.Code != contract.BadFlag || ce.Field != "since" {
			t.Errorf("%q: want BAD_FLAG on since, got %v", bad, err)
		}
	}
}
