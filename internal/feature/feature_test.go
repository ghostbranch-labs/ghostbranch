package feature_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
)

func TestValidateName(t *testing.T) {
	ok := []string{"feat-x", "auth/login", "v2.1_fix", "A1"}
	bad := []string{"", "-x", ".x", "x/", "x..y", "x//y", "x.lock", "x@{1}", "a b", "x/.y",
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
	passed := &feature.Gate{FeatureHead: "h", PublicMain: "p", Overlay: "o", Checks: feature.ChecksPassed, Leak: feature.LeakClean}
	failed := &feature.Gate{FeatureHead: "h", PublicMain: "p", Overlay: "o", Checks: feature.ChecksFailed, Leak: feature.LeakClean}
	in := func(m feature.Meta, head, pm, o string, pr bool) feature.StateInput {
		return feature.StateInput{Meta: m, Head: head, PublicMain: pm, Overlay: o, PublicPRExists: pr}
	}
	cases := []struct {
		name string
		in   feature.StateInput
		want feature.State
	}{
		{"draft", in(feature.Meta{}, "h", "p", "o", false), feature.StateDraft},
		{"gate passed", in(feature.Meta{Gate: passed}, "h", "p", "o", false), feature.StateGatePassed},
		{"stale: feature moved", in(feature.Meta{Gate: passed}, "h2", "p", "o", false), feature.StateInReview},
		{"stale: public moved", in(feature.Meta{Gate: passed}, "h", "p2", "o", false), feature.StateInReview},
		{"stale: overlay moved", in(feature.Meta{Gate: passed}, "h", "p", "o2", false), feature.StateInReview},
		{"failed", in(feature.Meta{Gate: failed}, "h", "p", "o", false), feature.StateInReview},
		{"promoted", in(feature.Meta{Gate: passed, Promoted: &feature.Promoted{PublicPR: 1}}, "h", "p", "o", true), feature.StatePromoted},
		{"promoted but PR gone", in(feature.Meta{Gate: passed, Promoted: &feature.Promoted{PublicPR: 1}}, "h", "p", "o", false), feature.StateGatePassed},
		{"abandoned wins", in(feature.Meta{Abandoned: true, Gate: passed}, "h", "p", "o", true), feature.StateAbandoned},
	}
	for _, c := range cases {
		if got := feature.StateOf(c.in); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestDecodeOfAnOlderMetadataFileHasNoCommits(t *testing.T) {
	old := `{"schema_version":"0.4","name":"x","branch":"x","created_by":"human:octo","created_at":"2026-10-05T12:00:00Z","base":"b",` +
		`"gate":{"feature_head":"h","public_main":"p","overlay":"o","candidate":"c","published_tree":"t","checks":"passed","leak":"clean","at":"2026-10-05T12:00:00Z"}}`
	m, err := feature.Decode([]byte(old))
	if err != nil || m.Gate == nil || m.Gate.Commits != nil {
		t.Fatalf("decode 0.4: %+v %v", m, err)
	}
}
