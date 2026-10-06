package guard

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
)

// Line is one line of git's pre-push input.
type Line struct{ LocalRef, LocalSHA, RemoteRef, RemoteSHA string }

// ParseInput reads git's pre-push input: one "<local ref> <local sha>
// <remote ref> <remote sha>" per line.
func ParseInput(r io.Reader) ([]Line, error) {
	var out []Line
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if len(f) != 4 {
			return nil, fmt.Errorf("unexpected pre-push input line %q", sc.Text())
		}
		out = append(out, Line{f[0], f[1], f[2], f[3]})
	}
	return out, sc.Err()
}

// Facts are what the pair's mirror knows about features.
type Facts struct {
	Names map[string]bool   // every feature
	Heads map[string]string // feature -> shadow head, for features with commits not in public main
}

// LoadFacts reads features from the engine's mirror, offline.
func LoadFacts(ctx context.Context, mirror *gitx.Git) (Facts, error) {
	f := Facts{Names: map[string]bool{}, Heads: map[string]string{}}
	metas, err := mirror.Refs(ctx, "refs/shadow-meta/features")
	if err != nil {
		return f, err
	}
	pm, hasPM, err := mirror.Resolve(ctx, "refs/remotes/public/main")
	if err != nil {
		return f, err
	}
	for ref := range metas {
		name := strings.TrimPrefix(ref, "refs/shadow-meta/features/")
		f.Names[name] = true
		head, ok, err := mirror.Resolve(ctx, "refs/remotes/shadow/"+name)
		if err != nil || !ok {
			continue
		}
		if hasPM {
			if in, err := mirror.IsAncestor(ctx, head, pm); err == nil && in {
				continue
			}
		}
		f.Heads[name] = head
	}
	return f, nil
}

// Block is a refused ref and why.
type Block struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
}

// CheckResult is `guard check` output when the push may go ahead.
type CheckResult struct {
	Checked int `json:"checked"`
}

func reserved(ref string, f Facts) string {
	b, ok := strings.CutPrefix(ref, "refs/heads/")
	switch {
	case !ok:
		return ""
	case b == "ghostbranch":
		return "it is the ghostbranch overlay branch, which never leaves the shadow repo"
	case strings.HasPrefix(b, "ghostbranch-gate/"):
		return "it is a ghostbranch gate branch, which never leaves the shadow repo"
	case f.Names[b]:
		return fmt.Sprintf("%q is a Ghostbranch feature", b)
	}
	return ""
}

// Check returns the pushed refs that would put feature work in the public
// repo (spec §11.4): by name, or by containing a feature's commits.
func Check(ctx context.Context, checkout *gitx.Git, lines []Line, f Facts) ([]Block, error) {
	names := make([]string, 0, len(f.Heads))
	for n := range f.Heads {
		names = append(names, n)
	}
	sort.Strings(names)
	var blocks []Block
	for _, l := range lines {
		if strings.Trim(l.LocalSHA, "0") == "" {
			continue // a deletion publishes nothing
		}
		why := reserved(l.RemoteRef, f)
		if why == "" {
			why = reserved(l.LocalRef, f)
		}
		for _, n := range names {
			if why != "" {
				break
			}
			head := f.Heads[n]
			if _, ok, err := checkout.Resolve(ctx, head); err != nil || !ok {
				continue // this clone never had the feature's commits
			}
			in, err := checkout.IsAncestor(ctx, head, l.LocalSHA)
			if err != nil {
				return nil, err
			}
			if in {
				why = fmt.Sprintf("it contains the commits of feature %q", n)
			}
		}
		if why != "" {
			blocks = append(blocks, Block{Ref: l.RemoteRef, Reason: why})
		}
	}
	return blocks, nil
}
