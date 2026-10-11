package engine_test

import (
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// editOps are the edit_pr ops of a preview.
func editOps(pv *preview.Preview) []preview.Op {
	var out []preview.Op
	if pv != nil {
		for _, o := range pv.Ops {
			if o["op"] == "edit_pr" {
				out = append(out, o)
			}
		}
	}
	return out
}

// I5: the preview of an update names every public write, the edit of the
// PR texts the person typed too: promote --dry-run, and apply --dry-run of
// the saved plan. With nothing typed there is no edit.
func TestUpdatePreviewNamesThePREdit(t *testing.T) {
	t.Parallel()
	h, pr := publishedFeature(t)
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
	h.gateFeature("feat-x")
	h.eng.DryRun = true
	res, err := promote(h, engine.PromoteOptions{Title: "New title"})
	if err != nil || res.Preview == nil {
		t.Fatalf("promote --dry-run = %+v %v", res, err)
	}
	validateResult(t, "promote.schema.json", res)
	if e := editOps(res.Preview); len(e) != 1 || e[0]["number"] != pr || e[0]["title"] != true || e[0]["body"] != false || e[0]["repo"] != publicName {
		t.Fatalf("edit ops = %+v in %+v", e, res.Preview.Ops)
	}
	if res, err = promote(h, engine.PromoteOptions{}); err != nil || len(editOps(res.Preview)) != 0 {
		t.Fatalf("nothing typed: %+v %v", res, err)
	}
	h.eng.DryRun = false
	saved, err := promote(h, engine.PromoteOptions{Body: "New body", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	h.eng.DryRun = true
	ar, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: saved.Plan.ID, Yes: true})
	if err != nil || ar.Preview == nil {
		t.Fatalf("apply --dry-run = %+v %v", ar, err)
	}
	validateResult(t, "apply.schema.json", ar)
	if e := editOps(ar.Preview); len(e) != 1 || e[0]["title"] != false || e[0]["body"] != true {
		t.Fatalf("apply edit ops = %+v", e)
	}
}

// I5, for a stack: the typed title of the feature listed is an edit of its
// PR in the stack plan's preview; the PRs above keep theirs.
func TestStackUpdatePreviewNamesThePREdit(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "a", "b")
	h.grant(time.Hour)
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "preserve", Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatal(err)
	}
	prA := res.Applied.PRs[0].Number
	testutil.Commit(t, h.shadowGit(), "a", "a second change", map[string]string{"a2.txt": "a2\n"})
	if _, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "a"}); err != nil {
		t.Fatal(err)
	}
	h.gateFeature("a")
	h.gateFeature("b")
	h.eng.DryRun = true
	res, _, err = promoteStack(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", Title: "Add a"})
	if err != nil || res.Preview == nil {
		t.Fatalf("promote a --dry-run = %+v %v", res, err)
	}
	if e := editOps(res.Preview); len(e) != 1 || e[0]["number"] != prA || e[0]["title"] != true || e[0]["body"] != false {
		t.Fatalf("edit ops = %+v in %+v", e, res.Preview.Ops)
	}
}
