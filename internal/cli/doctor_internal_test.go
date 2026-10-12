package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/updatecheck"
)

// A Ctrl-C'd doctor look is not a failed look: the saved cache keeps its
// checked_at and last_error, so the schedule is not pushed back.
func TestDoctorInterruptedLookLeavesTheCacheAlone(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel() // Ctrl-C while the request is in flight
		<-r.Context().Done()
	}))
	defer srv.Close()
	home := t.TempDir()
	vars := map[string]string{"GHOSTBRANCH_HOME": home, "GHOSTBRANCH_UPDATE_URL": srv.URL}
	path := filepath.Join(home, updatecheck.CacheFile)
	was := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if err := updatecheck.Save(path, updatecheck.Cache{CheckedAt: was, LastError: "earlier"}); err != nil {
		t.Fatal(err)
	}
	a := &app{env: Env{
		Getenv: func(k string) string { return vars[k] },
		Now:    func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) },
		Build:  func() (string, string) { return "v0.12.0", "0123456789ab" },
	}}
	a.doctorUpdateCheck(ctx)
	got := updatecheck.Load(path)
	if !got.CheckedAt.Equal(was) || got.LastError != "earlier" {
		t.Fatalf("cache = checked_at %v, last_error %q", got.CheckedAt, got.LastError)
	}
}
