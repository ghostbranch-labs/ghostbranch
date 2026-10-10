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
			`{"schema_version":"0.12","command":"doctor","ok":false,"exit_code":1,"data":{},"warnings":[],"next":[],"error":{"code":"DOCTOR_FAILED","message":"m","field":null,"retryable":false,"details":{"failed":["guard"]}}}`,
			`{"schema_version":"0.12","command":"auth status","ok":true,"exit_code":0,"data":{},"warnings":[{"code":"AGENT_ON_PERSON_LOGIN","message":"m"}],"next":[],"error":null}`,
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

// updatePlanJSON is planJSON with fields added to its publish object.
func updatePlanJSON(fields string) string {
	return strings.Replace(planJSON(""), `"dropped_empty":[]}`, `"dropped_empty":[]`+fields+`}`, 1)
}

// gateResultJSON is gate data, a first publish's, with extra fields added.
func gateResultJSON(extra string) string {
	g := `{"feature":"a","feature_head":"SHA","public_main":"SHA","parent":"","base":"SHA","overlay":"SHA","candidate":"SHA",` +
		`"published_tree":"SHA","scanned_as":"preserve","commits":[],"checks":"passed","failed_checks":[],"leak":"clean","findings":[],` +
		`"waived":[],"repo_guards":null,"shadow_pr":3,"job":"gate/a/0123456789ab","reused":false,"preview":null` + extra + `}`
	return strings.ReplaceAll(g, "SHA", sha40)
}

// gateRecordJSON is a feature record whose gate has extra fields added.
func gateRecordJSON(extra string) string {
	g := `{"feature_head":"SHA","public_main":"SHA","overlay":"SHA","candidate":"SHA","published_tree":"SHA",` +
		`"commits":[],"checks":"passed","leak":"clean","at":"2026-10-10T12:00:00Z"` + extra + `}`
	return strings.Replace(recordJSON(""), `"gate":null`, `"gate":`+strings.ReplaceAll(g, "SHA", sha40), 1)
}

// The 6.1a-2 fields: what Tasks 3 to 8 emit, and what they must never emit.
// All are optional, so plans and reports from before 0.12, and every first
// publish, still validate.
func TestSubProject61a2Schemas(t *testing.T) {
	t.Parallel()
	head := `,"expected_head":"` + sha40 + `"`
	emptyTitle := func(fields string) string {
		return strings.Replace(updatePlanJSON(fields), `"title":"Add a"`, `"title":""`, 1)
	}
	for name, raw := range map[string][]string{
		"plan.schema.json": {
			planJSON(""), // a first publish
			updatePlanJSON(`,"update":true,"style":"append"` + head),
			updatePlanJSON(`,"update":true,"style":"append","replace_reason":""` + head),
			updatePlanJSON(`,"update":true,"style":"replace","replace_reason":"restack"` + head),
			updatePlanJSON(`,"update":true,"style":"replace","replace_reason":"base_changed"` + head),
			updatePlanJSON(`,"update":true,"style":"replace","replace_reason":"curated"` + head),
			updatePlanJSON(`,"update":true,"style":"replace","replace_reason":"restack","restores":["` + sha40 + `"]` + head),
			// An update may leave the PR title empty (R12); a typed one is fine too.
			emptyTitle(`,"update":true,"style":"append"` + head),
			updatePlanJSON(`,"update":true,"style":"append"` + head),
			planJSON(`,"prs":[{"feature":"a","base":"main","number":14,"url":"u","update":true,"style":"replace"},` +
				`{"feature":"b","base":"a","number":15,"url":"u","update":true,"style":"append"}],"stack_number":null`),
			planJSON(`,"prs":[{"feature":"a","base":"main","number":14,"url":"u","update":true,"style":"append"},` +
				`{"feature":"b","base":"a","number":0,"url":""}],"stack_number":null`), // a new PR above an updated one
		},
		"apply.schema.json": {applyJSON(`,"prs":[{"feature":"a","base":"main","number":14,"url":"u","update":true,"style":"append"}],"stack_number":null`)},
		// A gate of an open PR's update (Task 4), as gate data and as the record.
		"gate.schema.json": {
			gateResultJSON(""), // a first publish
			gateResultJSON(`,"update":{"pr":14,"head":"` + sha40 + `","base":"main","style":"append"}`),
			gateResultJSON(`,"update":{"pr":14,"head":"` + sha40 + `","base":"main","style":"replace","replace_reason":"rebase"}`),
		},
		"feature.schema.json": {
			gateRecordJSON(""),
			gateRecordJSON(`,"base":"` + sha40 + `","update":{"pr":14,"head":"` + sha40 + `","base":"main","style":"append"}`),
			gateRecordJSON(`,"update":{"pr":14,"head":"` + sha40 + `","base":"release","style":"replace","replace_reason":"base_changed"}`),
		},
		"feature-report.schema.json": {
			reportJSON("pr_behind", `,"parent":"main","children":[],"stack":null,"pr":{"behind":2,"foreign":0}`),
			reportJSON("published", `,"parent":"main","children":[],"stack":null,"pr":{"behind":0,"foreign":1}`),
		},
		"envelope.schema.json": {
			`{"schema_version":"` + contract.SchemaVersion + `","command":"promote","ok":false,"exit_code":4,"data":{},"warnings":[],"next":[],` +
				`"error":{"code":"PUBLIC_BRANCH_CHANGED","message":"m","field":null,"retryable":false,` +
				`"details":{"feature":"a","foreign":[{"sha":"` + sha40 + `","author":"octo","subject":"Fix typo"}]}}}`,
			`{"schema_version":"` + contract.SchemaVersion + `","command":"feature pull","ok":true,"exit_code":0,"data":{},"warnings":[],"next":[],"error":null}`,
			`{"schema_version":"` + contract.SchemaVersion + `","command":"version","ok":true,"exit_code":0,"data":{},"warnings":[],"next":[],"error":null}`,
		},
		"ledger-entry.schema.json": {`{"ts":"2026-10-10T12:00:00Z","actor":"human:octo","op":"feature pull","exit_code":0,"summary":"s"}`},
	} {
		for _, r := range raw {
			testutil.ValidateSchema(t, name, []byte(r))
		}
	}
	for _, x := range []struct{ name, raw string }{
		{"plan.schema.json", updatePlanJSON(`,"update":true` + head)},                                               // no style
		{"plan.schema.json", updatePlanJSON(`,"update":true,"style":"append"`)},                                     // no lease
		{"plan.schema.json", updatePlanJSON(`,"style":"append"` + head)},                                            // style without update
		{"plan.schema.json", updatePlanJSON(`,"update":true,"style":"replace"` + head)},                             // replace without a reason
		{"plan.schema.json", updatePlanJSON(`,"update":true,"style":"replace","replace_reason":""` + head)},         // an empty reason
		{"plan.schema.json", updatePlanJSON(`,"update":true,"style":"append","replace_reason":"rebase"` + head)},    // append with a reason
		{"plan.schema.json", updatePlanJSON(`,"update":true,"style":"merge"` + head)},                               // unknown style
		{"plan.schema.json", updatePlanJSON(`,"update":true,"style":"replace","replace_reason":"other"` + head)},    // unknown reason
		{"plan.schema.json", updatePlanJSON(`,"update":true,"style":"append","expected_head":"abc"`)},               // not a sha
		{"plan.schema.json", updatePlanJSON(`,"update":true,"style":"append","restores":["` + sha40 + `"]` + head)}, // restores need a replace
		{"plan.schema.json", updatePlanJSON(`,"update":true,"style":"replace","replace_reason":"restack","restores":["abc"]` + head)},
		{"plan.schema.json", emptyTitle("")}, // a first publish needs a title
		{"plan.schema.json", planJSON(`,"prs":[{"feature":"a","base":"main","number":14,"url":"u","style":"append"}]`)},
		{"plan.schema.json", planJSON(`,"prs":[{"feature":"a","base":"main","number":14,"url":"u","update":true}]`)},
		{"gate.schema.json", gateResultJSON(`,"update":{"pr":14,"head":"` + sha40 + `","base":"main","style":"replace"}`)},                          // replace without a reason
		{"gate.schema.json", gateResultJSON(`,"update":{"pr":14,"head":"` + sha40 + `","base":"main","style":"append","replace_reason":"rebase"}`)}, // append with a reason
		{"gate.schema.json", gateResultJSON(`,"update":{"pr":14,"head":"` + sha40 + `","base":"main","style":"merge"}`)},                            // unknown style
		{"gate.schema.json", gateResultJSON(`,"update":{"pr":14,"head":"abc","base":"main","style":"append"}`)},                                     // not a sha
		{"gate.schema.json", gateResultJSON(`,"update":{"pr":0,"head":"` + sha40 + `","base":"main","style":"append"}`)},                            // no PR
		{"feature.schema.json", gateRecordJSON(`,"update":{"pr":14,"head":"` + sha40 + `","base":"main","style":"replace","replace_reason":"other"}`)},
		{"feature.schema.json", gateRecordJSON(`,"update":{"pr":14,"head":"` + sha40 + `","style":"append"}`)}, // no base branch
		{"feature.schema.json", gateRecordJSON(`,"update":{"pr":14,"head":"` + sha40 + `","base":"main","style":"replace"}`)},
		{"feature-report.schema.json", reportJSON("pr_behind", `,"parent":"main","children":[],"stack":null,"pr":{"behind":-1,"foreign":0}`)},
		{"feature-report.schema.json", reportJSON("pr_behind", `,"parent":"main","children":[],"stack":null,"pr":{"behind":1}`)},
		{"feature-report.schema.json", reportJSON("pr_behind", `,"parent":"main","children":[],"stack":null,"pr":{"behind":1,"foreign":0,"x":1}`)},
		{"feature-report.schema.json", reportJSON("behind", `,"parent":"main","children":[],"stack":null`)},
		{"ledger-entry.schema.json", `{"ts":"2026-10-10T12:00:00Z","actor":"human:octo","op":"feature push","exit_code":0,"summary":"s"}`},
	} {
		schemaRejects(t, x.name, x.raw)
	}
	// The schema's enums are the plan package's constants, exactly.
	var p struct {
		Properties struct {
			Publish struct {
				Properties struct {
					Style struct {
						Enum []string `json:"enum"`
					} `json:"style"`
					ReplaceReason struct {
						Enum []string `json:"enum"`
					} `json:"replace_reason"`
				} `json:"properties"`
			} `json:"publish"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaFile(t, "plan.schema.json"), &p); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(slices.Values(p.Properties.Publish.Properties.Style.Enum)); !slices.Equal(got, []string{plan.StyleAppend, plan.StyleReplace}) {
		t.Errorf("publish.style enum = %v", got)
	}
	reasons := []string{"", plan.ReasonBaseChanged, plan.ReasonConflict, plan.ReasonCurated, plan.ReasonRebase, plan.ReasonRestack, plan.ReasonReworded, plan.ReasonSquash}
	if got := slices.Sorted(slices.Values(p.Properties.Publish.Properties.ReplaceReason.Enum)); !slices.Equal(got, reasons) {
		t.Errorf("publish.replace_reason enum = %v, want %v", got, reasons)
	}
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
	// Task 3 adds feature.StatePRBehind with this string.
	if !slices.Contains(r.Properties.State.Enum, "pr_behind") {
		t.Errorf("feature-report state enum lacks pr_behind")
	}
}
