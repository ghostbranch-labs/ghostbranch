package ledger_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
)

func TestAppendIsOneLinePerEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "ledger.jsonl")
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	a := ledger.Entry{TS: at, Actor: "human:octo", Op: "sync", Pair: "acme/widget", Summary: "noop"}
	b := ledger.Entry{TS: at, Actor: "agent:claude-code", Op: "promote", ExitCode: 5, ErrorCode: "APPROVAL_REQUIRED", Summary: "needs a human"}
	if err := ledger.Append(path, a); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(path, b); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if n := strings.Count(string(raw), "\n"); n != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", n, raw)
	}
	got, err := ledger.Read(path)
	if err != nil || len(got) != 2 || !reflect.DeepEqual(got, []ledger.Entry{a, b}) {
		t.Fatalf("Read = %+v %v", got, err)
	}
	if strings.Contains(strings.Split(string(raw), "\n")[0], "plan_id") {
		t.Fatal("empty optional fields must be omitted")
	}
}

func TestReadMissingFileIsEmpty(t *testing.T) {
	got, err := ledger.Read(filepath.Join(t.TempDir(), "none.jsonl"))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v %v", got, err)
	}
}
