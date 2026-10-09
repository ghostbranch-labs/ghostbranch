package feature_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func TestValidateName(t *testing.T) {
	ok := []string{"feat-x", "auth/login", "v2.1_fix", "A1", "v1.lockstep/x"}
	bad := []string{"", "-x", ".x", "x/", "x..y", "x//y", "x.lock", "x@{1}", "a b", "x/.y", "a.lock/b", "x/y.lock/z",
		"ghostbranch", "ghostbranch/gate/x", "ghostbranch/x", "ghostbranch-gate/x", "main", "HEAD", strings.Repeat("a", 101)}
	for _, n := range ok {
		if err := feature.ValidateName(n, "name"); err != nil {
			t.Errorf("%q should be valid: %v", n, err)
		}
	}
	for _, n := range bad {
		err := feature.ValidateName(n, "name")
		if ce := contract.From(err); ce == nil || ce.Code != contract.NameInvalid || ce.Field != "name" {
			t.Errorf("%q should be NAME_INVALID on field name, got %v", n, err)
		}
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	m := feature.Meta{SchemaVersion: feature.SchemaVersion, Name: "x", Branch: "x", CreatedBy: "human:octo", CreatedAt: at, Base: "b",
		Gate:     &feature.Gate{FeatureHead: "h", PublicMain: "p", Overlay: "o", Candidate: "c", PublishedTree: "t", Checks: feature.ChecksPassed, Leak: feature.LeakClean, At: at},
		Promoted: &feature.Promoted{PublicPR: 7, PlanID: "id"}}
	got, err := feature.Decode(feature.Encode(m))
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if _, err := feature.Decode([]byte("{")); err == nil {
		t.Fatal("bad JSON must fail")
	}
}

func TestStateOf(t *testing.T) {
	t.Parallel()
	passed := &feature.Gate{FeatureHead: "h", PublicMain: "p", Overlay: "o", Checks: feature.ChecksPassed, Leak: feature.LeakClean}
	failed := &feature.Gate{FeatureHead: "h", PublicMain: "p", Overlay: "o", Checks: feature.ChecksFailed, Leak: feature.LeakClean}
	prom := &feature.Promoted{PublicPR: 1, FeatureHead: "h"}
	in := func(m feature.Meta, head, pm, o string, branch bool, pr string, moved bool) feature.StateInput {
		return feature.StateInput{Meta: m, BranchExists: branch, Head: head, PublicMain: pm, Overlay: o, PRState: pr, Moved: moved}
	}
	cases := []struct {
		name string
		in   feature.StateInput
		want feature.State
	}{
		{"draft", in(feature.Meta{}, "h", "p", "o", true, "", false), feature.StateDraft},
		{"gate passed", in(feature.Meta{Gate: passed}, "h", "p", "o", true, "", false), feature.StateGatePassed},
		{"stale: feature moved", in(feature.Meta{Gate: passed}, "h2", "p", "o", true, "", false), feature.StateInReview},
		{"stale: public moved", in(feature.Meta{Gate: passed}, "h", "p2", "o", true, "", false), feature.StateInReview},
		{"stale: overlay moved", in(feature.Meta{Gate: passed}, "h", "p", "o2", true, "", false), feature.StateInReview},
		{"failed", in(feature.Meta{Gate: failed}, "h", "p", "o", true, "", false), feature.StateInReview},
		{"published open", in(feature.Meta{Gate: passed, Promoted: prom}, "h", "p", "o", true, "open", false), feature.StatePublished},
		{"published unknown", in(feature.Meta{Gate: passed, Promoted: prom}, "h", "p", "o", true, "unknown", false), feature.StatePublished},
		{"published no state", in(feature.Meta{Promoted: prom}, "h", "p", "o", true, "", false), feature.StatePublished},
		{"merged", in(feature.Meta{Promoted: prom}, "h", "p", "o", true, "merged", false), feature.StateMerged},
		{"merged moved", in(feature.Meta{Promoted: prom}, "h", "p", "o", true, "merged", true), feature.StateMergedMoved},
		{"pr closed", in(feature.Meta{Promoted: prom}, "h", "p", "o", true, "closed", false), feature.StatePRClosed},
		{"merged state without promoted record", in(feature.Meta{Gate: passed}, "h", "p", "o", true, "merged", false), feature.StateGatePassed},
		{"branch gone", in(feature.Meta{Gate: passed, Promoted: prom}, "h", "p", "o", false, "merged", false), feature.StateBranchGone},
		{"abandoned wins", in(feature.Meta{Abandoned: true, Gate: passed}, "h", "p", "o", false, "open", false), feature.StateAbandoned},
	}
	for _, c := range cases {
		if got := feature.StateOf(c.in); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestDecodeOfA05FileHasNoFeatureHead(t *testing.T) {
	t.Parallel()
	old := `{"schema_version":"0.5","name":"x","branch":"x","created_by":"human:octo","created_at":"2026-10-05T12:00:00Z",` +
		`"base":"` + strings.Repeat("b", 40) + `","abandoned":false,"gate":null,` +
		`"promoted":{"public_pr":3,"plan_id":"20261005T120000Z-000001"}}`
	m, err := feature.Decode([]byte(old))
	if err != nil || m.Promoted == nil || m.Promoted.FeatureHead != "" || m.Promoted.PublicPR != 3 {
		t.Fatalf("decode 0.5: %+v %v", m, err)
	}
	// What a write of it puts in the shadow repo is a valid current record:
	// the engine stamps the version, and the missing feature_head stays absent.
	m.SchemaVersion = feature.SchemaVersion
	testutil.ValidateSchema(t, "feature.schema.json", feature.Encode(m))
}

func TestDecodeOfAnOlderMetadataFileHasNoCommits(t *testing.T) {
	old := `{"schema_version":"0.4","name":"x","branch":"x","created_by":"human:octo","created_at":"2026-10-05T12:00:00Z","base":"b",` +
		`"gate":{"feature_head":"h","public_main":"p","overlay":"o","candidate":"c","published_tree":"t","checks":"passed","leak":"clean","at":"2026-10-05T12:00:00Z"}}`
	m, err := feature.Decode([]byte(old))
	if err != nil || m.Gate == nil || m.Gate.Commits != nil {
		t.Fatalf("decode 0.4: %+v %v", m, err)
	}
}

func TestPendingGateRoundTrips(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	m := feature.Meta{SchemaVersion: feature.SchemaVersion, Name: "x", Branch: "x", Gate: &feature.Gate{Candidate: "c",
		Checks: feature.ChecksPending, Leak: feature.LeakClean, ShadowPR: 7, StartedAt: at, At: at}}
	got, err := feature.Decode(feature.Encode(m))
	if err != nil || !got.Gate.Pending() || got.Gate.ShadowPR != 7 || !got.Gate.StartedAt.Equal(at) || got.Gate.Passed() {
		t.Fatalf("round trip = %+v %v", got.Gate, err)
	}
	// A 0.6 record has none of the new fields, and they stay absent when re-encoded.
	old, _ := feature.Decode([]byte(`{"gate":{"candidate":"c","checks":"passed","leak":"clean","at":"2026-10-05T12:00:00Z"}}`))
	if old.Gate.Pending() || old.Gate.ShadowPR != 0 || !old.Gate.StartedAt.IsZero() {
		t.Fatalf("old record = %+v", old.Gate)
	}
	if s := string(feature.Encode(old)); strings.Contains(s, "started_at") || strings.Contains(s, "shadow_pr") || strings.Contains(s, "failed_checks") {
		t.Fatalf("empty new fields were written: %s", s)
	}
	var nilGate *feature.Gate
	if nilGate.Pending() {
		t.Fatal("a nil gate is pending")
	}
}

func TestAPendingRecordWithoutAShadowPRIsNoJob(t *testing.T) {
	t.Parallel()
	g := &feature.Gate{Candidate: "c", Checks: feature.ChecksPending} // an older binary rewrote it
	if g.Pending() {
		t.Fatal("a pending record without shadow_pr counts as a job")
	}
	g.ShadowPR = 7
	if !g.Pending() {
		t.Fatal("a pending record with its PR is not a job")
	}
}
