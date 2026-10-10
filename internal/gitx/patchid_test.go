package gitx_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// The same change replayed onto another base keeps its patch-id: that is how
// sync tells GitHub's restack of a public branch from someone's push.
func TestPatchIDsMatchAcrossBases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := testutil.BareRepo(t, filepath.Join(t.TempDir(), "r.git"))
	base := testutil.Commit(t, g, "main", "base", map[string]string{"f.txt": "f\n"})
	if err := g.UpdateRef(ctx, "refs/heads/one", base); err != nil {
		t.Fatal(err)
	}
	b1 := testutil.Commit(t, g, "one", "add b", map[string]string{"b.txt": "b\n"})
	other := testutil.Commit(t, g, "main", "upstream", map[string]string{"o.txt": "o\n"})
	if err := g.UpdateRef(ctx, "refs/heads/two", other); err != nil {
		t.Fatal(err)
	}
	b2 := testutil.Commit(t, g, "two", "add b, reworded", map[string]string{"b.txt": "b\n"})
	b3 := testutil.Commit(t, g, "two", "change b", map[string]string{"b.txt": "changed\n"})
	empty := testutil.Commit(t, g, "two", "nothing", map[string]string{})

	ids, err := g.PatchIDs(ctx, []string{b1, b2, b3, empty})
	if err != nil {
		t.Fatal(err)
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("the same change has patch-ids %q and %q", ids[0], ids[1])
	}
	if ids[2] == "" || ids[2] == ids[0] {
		t.Fatalf("another change has patch-id %q", ids[2])
	}
	if ids[3] != "" {
		t.Fatalf("an empty commit has patch-id %q, want empty", ids[3])
	}
	got, err := g.CommitsBetween(ctx, other, empty)
	if err != nil || !reflect.DeepEqual(got, []string{b2, b3, empty}) {
		t.Fatalf("CommitsBetween = %v %v", got, err)
	}
	if got, err := g.CommitsBetween(ctx, empty, empty); err != nil || len(got) != 0 || got == nil {
		t.Fatalf("CommitsBetween of nothing = %#v %v, want an empty list", got, err)
	}
}
