package engine

// The --curate file: an agent's grouping, order and messages for the
// public commits of a curated promote.

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
)

var shaPrefix = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

type curateFile struct {
	Commits []plan.Group `json:"commits"`
}

func curateInvalid(format string, args ...any) *contract.Error {
	return contract.New(contract.CurateInvalid, format, args...).WithField("curate")
}

// LoadCurate reads a --curate file and checks its shape: at least one
// group, each naming one or more commits by SHA (7 to 40 lowercase hex
// characters) with a non-empty message. Whether the SHAs are the feature's
// commits is ResolveGroups' job.
func LoadCurate(path string) ([]plan.Group, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, contract.Wrap(contract.BadFlag, err, "read the --curate file").WithField("curate")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f curateFile
	if err := dec.Decode(&f); err != nil {
		return nil, curateInvalid("the --curate file is not {\"commits\": [{\"from\": [<sha>...], \"message\": \"...\"}]}: %v", err)
	}
	if len(f.Commits) == 0 {
		return nil, curateInvalid("the --curate file lists no commits")
	}
	for i, g := range f.Commits {
		if len(g.From) == 0 {
			return nil, curateInvalid("group %d names no commits", i)
		}
		if strings.TrimSpace(g.Message) == "" {
			return nil, curateInvalid("group %d has an empty message", i)
		}
		for _, s := range g.From {
			if !shaPrefix.MatchString(s) {
				return nil, curateInvalid("group %d: %q is not a commit SHA (7 to 40 lowercase hex characters)", i, s)
			}
		}
		f.Commits[i].Message = strings.TrimSpace(g.Message)
	}
	return f.Commits, nil
}

// ResolveGroups maps each SHA in groups to the one feature commit it names,
// and checks the groups name every commit exactly once.
func ResolveGroups(groups []plan.Group, sources []string) ([]plan.Group, error) {
	seen := map[string]bool{}
	missing, repeated, unknown, ambiguous := []string{}, []string{}, []string{}, []string{}
	out := make([]plan.Group, len(groups))
	for i, g := range groups {
		out[i] = plan.Group{Message: g.Message, From: make([]string, 0, len(g.From))}
		for _, p := range g.From {
			var hits []string
			for _, s := range sources {
				if strings.HasPrefix(s, p) {
					hits = append(hits, s)
				}
			}
			switch {
			case len(hits) == 0:
				unknown = append(unknown, p)
			case len(hits) > 1:
				ambiguous = append(ambiguous, p)
			case seen[hits[0]]:
				if !slices.Contains(repeated, hits[0]) {
					repeated = append(repeated, hits[0])
				}
			default:
				seen[hits[0]] = true
				out[i].From = append(out[i].From, hits[0])
			}
		}
	}
	for _, s := range sources {
		if !seen[s] {
			missing = append(missing, s)
		}
	}
	if len(missing)+len(repeated)+len(unknown)+len(ambiguous) == 0 {
		return out, nil
	}
	for _, l := range [][]string{missing, repeated, unknown, ambiguous} {
		sort.Strings(l)
	}
	return nil, curateInvalid("the curate groups must name each of the feature's %d commits exactly once", len(sources)).
		WithDetails(map[string]any{"missing": missing, "repeated": repeated, "unknown": unknown, "ambiguous": ambiguous})
}
