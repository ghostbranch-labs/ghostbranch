package contract_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
	"github.com/ghostbranch-labs/ghostbranch/internal/waiver"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

func schemaFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "schemas", "v0", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The envelope schema's enums are the registry's codes, exactly; every
// record's schema_version const is this binary's version.
func TestEnvelopeSchemaMatchesTheContract(t *testing.T) {
	t.Parallel()
	var s struct {
		Properties struct {
			SchemaVersion struct {
				Const string `json:"const"`
			} `json:"schema_version"`
			Command struct {
				Enum []string `json:"enum"`
			} `json:"command"`
			Warnings struct {
				Items struct {
					Properties struct {
						Code struct {
							Enum []string `json:"enum"`
						} `json:"code"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"warnings"`
			Error struct {
				OneOf []struct {
					Properties struct {
						Code struct {
							Enum []string `json:"enum"`
						} `json:"code"`
					} `json:"properties"`
				} `json:"oneOf"`
			} `json:"error"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaFile(t, "envelope.schema.json"), &s); err != nil {
		t.Fatal(err)
	}
	if s.Properties.SchemaVersion.Const != contract.SchemaVersion {
		t.Errorf("envelope schema_version const %q, want %q", s.Properties.SchemaVersion.Const, contract.SchemaVersion)
	}
	var codes []string
	for _, c := range contract.AllCodes() {
		codes = append(codes, string(c))
	}
	if len(s.Properties.Error.OneOf) != 2 {
		t.Fatalf("error.oneOf has %d branches", len(s.Properties.Error.OneOf))
	}
	got := slices.Sorted(slices.Values(s.Properties.Error.OneOf[1].Properties.Code.Enum))
	if !slices.Equal(got, codes) {
		t.Errorf("envelope error codes %v\nwant %v", got, codes)
	}
	var warns []string
	for _, w := range contract.AllWarnings() {
		warns = append(warns, string(w))
	}
	if got := slices.Sorted(slices.Values(s.Properties.Warnings.Items.Properties.Code.Enum)); !slices.Equal(got, warns) {
		t.Errorf("envelope warning codes %v\nwant %v", got, warns)
	}
	for _, cmd := range []string{"login", "logout", "auth status", "doctor"} {
		if !slices.Contains(s.Properties.Command.Enum, cmd) {
			t.Errorf("envelope command enum lacks %q", cmd)
		}
	}
	for _, name := range []string{"feature.schema.json", "grants.schema.json", "plan.schema.json", "waivers.schema.json", "wait-event.schema.json"} {
		var r struct {
			Properties struct {
				SchemaVersion struct {
					Const string `json:"const"`
				} `json:"schema_version"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(schemaFile(t, name), &r); err != nil || r.Properties.SchemaVersion.Const != contract.SchemaVersion {
			t.Errorf("%s schema_version const %q (%v), want %q", name, r.Properties.SchemaVersion.Const, err, contract.SchemaVersion)
		}
	}
}

func TestRecordVersionsMatchTheContract(t *testing.T) {
	t.Parallel()
	for name, v := range map[string]string{"feature": feature.SchemaVersion, "grant": grant.SchemaVersion, "plan": plan.SchemaVersion,
		"waiver": waiver.SchemaVersion, "workspace": workspace.SchemaVersion} {
		if v != contract.SchemaVersion {
			t.Errorf("%s.SchemaVersion = %q, want %q", name, v, contract.SchemaVersion)
		}
	}
}

// schemaRejects fails t when raw matches schemas/v0/<name>.
func schemaRejects(t *testing.T, name, raw string) {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "schemas", "v0", name))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	s, err := c.Compile(path)
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if s.Validate(inst) == nil {
		t.Errorf("%s accepted %s", name, raw)
	}
}

const initBase = `"public":"acme/widget","shadow":"acme/widget-shadow","created_shadow":true,"adopted":false,"resumed":false,` +
	`"mirrored_branches":["main"],"mirrored_tags":[],"overlay_created":true,"default_branch_set":true,"audit":null,"preview":null`

// The 6.1 additions to existing results, and the envelope's new values: what
// the later tasks must emit, and what they must never emit.
func TestSubProject61Schemas(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string][]string{
		"init.schema.json": {
			`{` + initBase + `,"guard_installed":false,"doctor":null}`,
			`{` + initBase + `,"guard_installed":true,"doctor":{"ok":5,"warn":0,"fail":1,"skipped":0,"failed":["guard"]}}`,
		},
		"agents-status.schema.json": {
			`{"pair":"acme/widget","grants":[],"token_can_publish":"false","token":{"source":"GHOSTBRANCH_TOKEN","http_status":403},"token_kind":"fine-grained"}`,
			`{"pair":"acme/widget","grants":[],"token_can_publish":"false","token":{"source":"GHOSTBRANCH_TOKEN","http_status":403},"token_kind":"unknown"}`,
		},
		"envelope.schema.json": {
			`{"schema_version":"0.11","command":"doctor","ok":false,"exit_code":1,"data":{},"warnings":[],"next":[],"error":{"code":"DOCTOR_FAILED","message":"m","field":null,"retryable":false,"details":{"failed":["guard"]}}}`,
			`{"schema_version":"0.11","command":"auth status","ok":true,"exit_code":0,"data":{},"warnings":[{"code":"AGENT_ON_PERSON_LOGIN","message":"m"}],"next":[],"error":null}`,
		},
	} {
		for _, r := range raw {
			testutil.ValidateSchema(t, name, []byte(r))
		}
	}
	for name, raw := range map[string]string{
		"init.schema.json":          `{` + initBase + `,"guard_installed":false,"doctor":{"ok":1,"warn":0,"fail":0,"skipped":0,"failed":["nope"]}}`,
		"agents-status.schema.json": `{"pair":"acme/widget","grants":[],"token_can_publish":"false","token":{"source":"GHOSTBRANCH_TOKEN","http_status":403},"token_kind":"keychain"}`,
	} {
		schemaRejects(t, name, raw)
	}
	// guard_installed and doctor are always present: doctor is null unless
	// a guided init ran it.
	schemaRejects(t, "init.schema.json", `{`+initBase+`}`)
}

const sha40 = "0123456789abcdef0123456789abcdef01234567"

func recordJSON(extra string) string {
	return `{"schema_version":"` + contract.SchemaVersion + `","name":"b","branch":"b","created_by":"human:octo",` +
		`"created_at":"2026-10-10T12:00:00Z","base":"` + sha40 + `","abandoned":false,"gate":null,"promoted":null` + extra + `}`
}

func reportJSON(state, extra string) string {
	return `{"name":"b","state":"` + state + `","branch":"b","head":"` + sha40 + `","base":"` + sha40 + `",` +
		`"drift":{"status":"clean","files":[]},"gate":null,"public_pr":null,"pr_state":"","plan_id":"","moved":false,"abandoned":false` + extra + `}`
}

func applyJSON(extra string) string {
	return `{"plan_id":"20261010T120000Z-0a0b0c","public_branch":"b","public_pr":12,"pr_url":"https://github.com/acme/widget/pull/12",` +
		`"resumed":false,"approval":{"kind":"console"},"waived":[],"preview":null` + extra + `}`
}

func promoteJSON(extra string) string {
	return `{"gate":null,"plan":null,"plan_path":"","findings":[],"waived":[],"applied":null,"preview":null` + extra + `}`
}

func planJSON(extra string) string {
	p := `{"schema_version":"` + contract.SchemaVersion + `","id":"20261010T120000Z-0a0b0c","kind":"promote","actor":"human:octo",` +
		`"created_at":"2026-10-10T12:00:00Z","pair":{"public":"acme/widget","shadow":"acme/widget-shadow"},"feature":"a","mode":"squash",` +
		`"fingerprint":{"public_main":"SHA","feature_head":"SHA","overlay":"SHA","gate_candidate":"SHA"},"curate":null,` +
		`"publish":{"commit":"SHA","tree":"SHA","parent":"SHA","target_branch":"a","pr":{"title":"Add a","body":""},` +
		`"commits":[{"commit":"SHA","tree":"SHA","parent":"SHA","sources":["SHA"],` +
		`"author":{"name":"octo","email":"42+octo@users.noreply.github.com","date":"2026-10-10T12:00:00Z"},"subject":"Add a"}],"dropped_empty":[]},` +
		`"typed":{"title":false,"body":false},"leak":{"findings":[],"waived":[],"warnings":[],"scanner_version":"gitleaks v8.30.1"},` +
		`"ci":{"repo_guards":{"mode":"rewrite","rewritten":[],"unrewritable":[],"skipped":[]}}` + extra + `}`
	return strings.ReplaceAll(p, "SHA", sha40)
}

// stackPlanJSON is a plan of two PRs: its chain repeats the top-level
// shapes, entry by entry, with mode as given for the second.
func stackPlanJSON(mode ...string) string {
	m := "preserve"
	if len(mode) > 0 {
		m = mode[0]
	}
	entry := func(f, mode string) string {
		e := `{"feature":"` + f + `","mode":"` + mode + `",` +
			`"fingerprint":{"public_main":"SHA","feature_head":"SHA","overlay":"SHA","gate_candidate":"SHA"},` +
			`"publish":{"commit":"SHA","tree":"SHA","parent":"SHA","target_branch":"` + f + `","pr":{"title":"Add ` + f + `","body":""},` +
			`"commits":[{"commit":"SHA","tree":"SHA","parent":"SHA","sources":["SHA"],` +
			`"author":{"name":"octo","email":"42+octo@users.noreply.github.com","date":"2026-10-10T12:00:00Z"},"subject":"Add ` + f + `"}],"dropped_empty":[]},` +
			`"typed":{"title":false,"body":false},"ci":{"repo_guards":{"mode":"rewrite","rewritten":[],"unrewritable":[],"skipped":[]}}}`
		return strings.ReplaceAll(e, "SHA", sha40)
	}
	return planJSON(`,"chain":[` + entry("a", "squash") + `,` + entry("b", m) + `]` + plannedPRs)
}

const (
	openedPRs = `,"prs":[{"feature":"a","base":"main","number":11,"url":"https://github.com/acme/widget/pull/11"},` +
		`{"feature":"b","base":"a","number":12,"url":"https://github.com/acme/widget/pull/12"}],"stack_number":13`
	plannedPRs = `,"prs":[{"feature":"a","base":"main","number":0,"url":""},{"feature":"b","base":"a","number":0,"url":""}],"stack_number":null`
)

// The 6.1a fields: what Tasks 3 to 8 emit, and what they must never emit.
// Each is optional until the task that emits it makes it required, so
// records and results from before 0.11 still validate.
func TestSubProject61aSchemas(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string][]string{
		"feature.schema.json": {
			recordJSON(""), // a record from before 0.11: parent main
			recordJSON(`,"parent":"a","forked_at":"` + sha40 + `"`),
			strings.Replace(recordJSON(""), `"promoted":null`, `"promoted":{"public_pr":2,"plan_id":"20261010T120000Z-0a0b0c","branch":"b","base":"a",`+
				`"published_head":"`+sha40+`","patch_ids":["p1"],"stack":4}`, 1),
		},
		"feature-report.schema.json": {
			reportJSON("draft", `,"parent":"main","children":[],"stack":null`),
			reportJSON("parent_stale", `,"parent":"a","children":["c"],"stack":null`),
			reportJSON("public_pr_needs_update", `,"parent":"main","children":[],"stack":13`),
			reportJSON("unreadable", `,"parent":"","children":[],"stack":null,"error":"x"`),
		},
		"apply.schema.json":   {applyJSON(openedPRs), applyJSON(`,"prs":[],"stack_number":null`)},
		"promote.schema.json": {promoteJSON(openedPRs)},
		"plan.schema.json":    {planJSON(""), planJSON(plannedPRs), stackPlanJSON()},
		"envelope.schema.json": {
			`{"schema_version":"` + contract.SchemaVersion + `","command":"feature fold","ok":false,"exit_code":4,"data":{},"warnings":[],"next":[],` +
				`"error":{"code":"FOLD_NEEDS_REBASE","message":"m","field":null,"retryable":false,"details":{"parent":"a"}}}`,
			`{"schema_version":"` + contract.SchemaVersion + `","command":"feature restack","ok":true,"exit_code":0,"data":{},"warnings":[],"next":[],"error":null}`,
			`{"schema_version":"` + contract.SchemaVersion + `","command":"promote","ok":false,"exit_code":2,"data":{},"warnings":[],"next":[],` +
				`"error":{"code":"NOT_A_STACK","message":"m","field":null,"retryable":false,"details":{"reason":"gap","feature":"c"}}}`,
			`{"schema_version":"` + contract.SchemaVersion + `","command":"sync","ok":true,"exit_code":0,"data":{},"warnings":[{"code":"PUBLIC_BRANCH_CHANGED","message":"m"}],"next":[],"error":null}`,
		},
		"ledger-entry.schema.json": {
			`{"ts":"2026-10-10T12:00:00Z","actor":"human:octo","op":"feature fold","exit_code":0,"summary":"s"}`,
			`{"ts":"2026-10-10T12:00:00Z","actor":"human:octo","op":"feature restack","exit_code":0,"summary":"s"}`,
		},
	} {
		for _, r := range raw {
			testutil.ValidateSchema(t, name, []byte(r))
		}
	}
	for _, x := range []struct{ name, raw string }{
		{"feature.schema.json", recordJSON(`,"parent":""`)},
		{"feature.schema.json", recordJSON(`,"parent":"a","forked_at":"not-a-sha"`)},
		{"feature-report.schema.json", reportJSON("stacked", `,"parent":"main","children":[],"stack":null`)},
		{"feature-report.schema.json", reportJSON("draft", `,"parent":"main","children":[],"stack":0`)},
		{"feature-report.schema.json", reportJSON("draft", `,"parent":"main","children":"c","stack":null`)},
		// parent, children and stack are required from 0.11 (Task 8).
		{"feature-report.schema.json", reportJSON("draft", "")},
		{"feature-report.schema.json", reportJSON("draft", `,"children":[],"stack":null`)},
		{"feature-report.schema.json", reportJSON("draft", `,"parent":"main","stack":null`)},
		{"feature-report.schema.json", reportJSON("draft", `,"parent":"main","children":[]`)},
		// prs and stack_number are required in results from 0.11 (Task 6); plans
		// keep them optional, as plan files from before 0.11 lack them.
		{"apply.schema.json", applyJSON("")},
		{"promote.schema.json", promoteJSON("")},
		{"apply.schema.json", applyJSON(`,"prs":[],"stack_number":0`)},
		{"apply.schema.json", applyJSON(`,"prs":[{"feature":"a","number":11,"url":""}],"stack_number":null`)},
		{"apply.schema.json", applyJSON(`,"prs":[{"feature":"a","base":"main","number":11,"url":"","sha":"` + sha40 + `"}],"stack_number":null`)},
		{"plan.schema.json", planJSON(`,"prs":[{"feature":"a","base":"main","number":-1,"url":""}]`)},
		{"plan.schema.json", stackPlanJSON("rebase")},
		{"plan.schema.json", planJSON(`,"chain":[]`)},
		{"envelope.schema.json", `{"schema_version":"` + contract.SchemaVersion + `","command":"feature stack","ok":true,"exit_code":0,"data":{},"warnings":[],"next":[],"error":null}`},
	} {
		schemaRejects(t, x.name, x.raw)
	}
	// The report's state enum names both new states.
	var r struct {
		Properties struct {
			State struct {
				Enum []string `json:"enum"`
			} `json:"state"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaFile(t, "feature-report.schema.json"), &r); err != nil {
		t.Fatal(err)
	}
	for _, s := range []feature.State{feature.StateParentStale, feature.StatePublicPRNeedsUpdate} {
		if !slices.Contains(r.Properties.State.Enum, string(s)) {
			t.Errorf("feature-report state enum lacks %q", s)
		}
	}
}
