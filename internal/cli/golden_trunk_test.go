package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// mainWord is the word main in output: on a pair whose base branch is
// trunk, nothing a person or agent reads may say it.
var mainWord = regexp.MustCompile(`\bmain\b`)

// TestGoldenTrunkPair is the golden contract on a pair whose public repo's
// default branch is trunk (roadmap 8.12): init, status, feature list and
// show, feature start, sync, doctor and a restack next, each in text and
// --json. The text never says main, and the JSON names the base branch.
func TestGoldenTrunkPair(t *testing.T) {
	t.Parallel()
	scenarios := []struct {
		name string
		prep func(*testEnv)
		args []string
		exit int
	}{
		{"trunk_init", nil, []string{"init", "acme/widget"}, 0},
		{"trunk_sync", func(te *testEnv) {
			initOnly(te)
			testutil.Commit(te.t, te.pubGit, "trunk", "upstream", map[string]string{"up.txt": "up\n"})
		}, []string{"sync"}, 0},
		{"trunk_feature_start", initOnly, []string{"feature", "start", "feat-x"}, 0},
		{"trunk_feature_start_behind", func(te *testEnv) {
			initOnly(te)
			testutil.Commit(te.t, te.pubGit, "trunk", "upstream", map[string]string{"up.txt": "up\n"})
		}, []string{"feature", "start", "feat-x"}, 4},
		{"trunk_status", withStack, []string{"status"}, 0},
		{"trunk_feature_list", withStack, []string{"feature", "list"}, 0},
		{"trunk_feature_show", withFeature, []string{"feature", "show", "feat-x"}, 0},
		{"trunk_doctor", initOnly, []string{"doctor", "--repo", "acme/widget"}, 0},
		{"trunk_restack_next", withStack, []string{"feature", "abandon", "feat-x"}, contract.HasChildren.Exit()},
		{"trunk_restack_onto", withStack, []string{"feature", "restack", "feat-y", "--onto", "trunk"}, 0},
		// R8: a name that is neither the base nor a feature is BAD_FLAG on
		// onto, and next lists the features (as FEATURE_NOT_FOUND's did).
		{"trunk_restack_onto_unknown", withStack, []string{"feature", "restack", "feat-y", "--onto", "master"}, contract.BadFlag.Exit()},
	}
	for _, s := range scenarios {
		for _, asJSON := range []bool{true, false} {
			name, ext := s.name, ".txt"
			if asJSON {
				ext = ".json"
			}
			t.Run(name+ext, func(t *testing.T) {
				t.Parallel()
				te := newTestEnvOn(t, "trunk")
				if s.prep != nil {
					s.prep(te)
				}
				args := s.args
				if asJSON {
					args = append(append([]string(nil), args...), "--json")
				}
				code, out, errOut := te.run(args...)
				if code != s.exit {
					t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, s.exit, out, errOut)
				}
				got := normalize(out, te.root)
				if asJSON {
					testutil.ValidateSchema(t, "envelope.schema.json", []byte(out))
					var env struct {
						SchemaVersion string          `json:"schema_version"`
						Command       string          `json:"command"`
						Data          json.RawMessage `json:"data"`
					}
					if err := json.Unmarshal([]byte(out), &env); err != nil {
						t.Fatal(err)
					}
					if env.SchemaVersion != "0.17" {
						t.Errorf("schema_version = %q, want 0.17", env.SchemaVersion)
					}
					if string(env.Data) != "{}" {
						testutil.ValidateSchema(t, dataSchemas[env.Command], env.Data)
					}
					var data map[string]any
					_ = json.Unmarshal(env.Data, &data)
					if _, has := data["base_branch"]; has && data["base_branch"] != "trunk" {
						t.Errorf("base_branch = %v, want trunk", data["base_branch"])
					}
					switch env.Command {
					case "init", "status", "feature list", "feature show", "feature start", "sync", "doctor":
						if string(env.Data) != "{}" && data["base_branch"] != "trunk" {
							t.Errorf("%s has no base_branch trunk: %s", env.Command, env.Data)
						}
					}
					// next and the error are read too: neither may say main.
					var said struct {
						Next  []contract.Next `json:"next"`
						Error *struct {
							Message string `json:"message"`
						} `json:"error"`
					}
					_ = json.Unmarshal([]byte(out), &said)
					for _, n := range said.Next {
						if mainWord.MatchString(n.Why) || mainWord.MatchString(strings.Join(n.Command, " ")) {
							t.Errorf("a next step says main: %+v", n)
						}
					}
					if said.Error != nil && mainWord.MatchString(said.Error.Message) {
						t.Errorf("the error says main: %s", said.Error.Message)
					}
				} else {
					got += normalize(errOut, te.root)
					for i, line := range strings.Split(got, "\n") {
						if mainWord.MatchString(line) {
							t.Errorf("text line %d says main: %q", i+1, line)
						}
					}
				}
				golden := filepath.Join("testdata", "golden", name+ext)
				if *update {
					if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatalf("missing golden %s; run go test ./internal/cli -run Golden -update", golden)
				}
				if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
					t.Fatalf("output differs from %s\n--- got\n%s\n--- want\n%s", golden, got, want)
				}
			})
		}
	}
}
