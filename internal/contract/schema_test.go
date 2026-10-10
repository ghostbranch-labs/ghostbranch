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
			`{"pair":"acme/widget","grants":[],"token_can_publish":"false","token":{"source":"GHOSTBRANCH_TOKEN","http_status":403}}`,
		},
		"envelope.schema.json": {
			`{"schema_version":"0.10","command":"doctor","ok":false,"exit_code":1,"data":{},"warnings":[],"next":[],"error":{"code":"DOCTOR_FAILED","message":"m","field":null,"retryable":false,"details":{"failed":["guard"]}}}`,
			`{"schema_version":"0.10","command":"auth status","ok":true,"exit_code":0,"data":{},"warnings":[{"code":"AGENT_ON_PERSON_LOGIN","message":"m"}],"next":[],"error":null}`,
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
