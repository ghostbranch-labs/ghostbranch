package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Identity is an author or committer with a fixed time, so commits made
// from the same inputs always get the same SHA.
type Identity struct {
	Name  string
	Email string
	When  time.Time
}

func (id Identity) env(prefix string) []string {
	date := strconv.FormatInt(id.When.Unix(), 10) + " +0000"
	return []string{prefix + "_NAME=" + id.Name, prefix + "_EMAIL=" + id.Email, prefix + "_DATE=" + date}
}

// TreeEntry is one file (blob) or submodule (commit) in a flattened tree.
type TreeEntry struct {
	Mode string
	Type string
	SHA  string
	Path string
}

// HashObject stores content as a blob, byte for byte.
func (g *Git) HashObject(ctx context.Context, content []byte) (string, error) {
	return g.RunInput(ctx, bytes.NewReader(content), "hash-object", "-w", "--no-filters", "--stdin")
}

// MakeTree writes a tree from flattened entries and returns its SHA.
func (g *Git) MakeTree(ctx context.Context, entries []TreeEntry) (string, error) {
	tmp, err := os.MkdirTemp("", "ghostbranch-index-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	ig := g.With(g.Dir)
	ig.Env = append(ig.Env, "GIT_INDEX_FILE="+filepath.Join(tmp, "index"))
	var in bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&in, "%s %s\t%s\x00", e.Mode, e.SHA, e.Path)
	}
	if _, err := ig.RunInput(ctx, &in, "update-index", "-z", "--add", "--index-info"); err != nil {
		return "", err
	}
	return ig.Run(ctx, "write-tree")
}

// ListTree flattens rev's tree into its files and submodules, sorted by path.
func (g *Git) ListTree(ctx context.Context, rev string) ([]TreeEntry, error) {
	out, err := g.runRaw(ctx, nil, "ls-tree", "-r", "-z", "--full-tree", rev)
	if err != nil {
		return nil, err
	}
	var es []TreeEntry
	for _, rec := range splitNUL(out) {
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("ls-tree: bad record %q", rec)
		}
		es = append(es, TreeEntry{Mode: f[0], Type: f[1], SHA: f[2], Path: path})
	}
	return es, nil
}

// TreeOf returns the tree SHA of a commit or tree.
func (g *Git) TreeOf(ctx context.Context, rev string) (string, error) {
	return g.MustResolve(ctx, rev+"^{tree}")
}

// CommitTree writes a commit object. The message is stored with exactly one
// trailing newline.
func (g *Git) CommitTree(ctx context.Context, tree string, parents []string, message string, author, committer Identity) (string, error) {
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	args = append(args, "-F", "-")
	cg := g.With(g.Dir)
	cg.Env = append(cg.Env, author.env("GIT_AUTHOR")...)
	cg.Env = append(cg.Env, committer.env("GIT_COMMITTER")...)
	return cg.RunInput(ctx, strings.NewReader(strings.TrimRight(message, "\n")+"\n"), args...)
}

// CommitMessage returns a commit's message exactly as stored: the raw object
// after the header's blank line. Unlike log --format=%B, no config (such as
// i18n.logOutputEncoding) can re-encode it.
func (g *Git) CommitMessage(ctx context.Context, sha string) (string, error) {
	out, err := g.runRaw(ctx, nil, "cat-file", "commit", sha)
	if err != nil {
		return "", err
	}
	_, msg, ok := bytes.Cut(out, []byte("\n\n"))
	if !ok {
		return "", nil // a commit with an empty message has no body separator
	}
	return string(msg), nil
}

// ReadBlobs returns the content of each blob, keyed by SHA.
func (g *Git) ReadBlobs(ctx context.Context, shas []string) (map[string][]byte, error) {
	res := map[string][]byte{}
	if len(shas) == 0 {
		return res, nil
	}
	out, err := g.runRaw(ctx, strings.NewReader(strings.Join(shas, "\n")+"\n"), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	for len(out) > 0 {
		nl := bytes.IndexByte(out, '\n')
		if nl < 0 {
			return nil, fmt.Errorf("cat-file: truncated header")
		}
		header := strings.Fields(string(out[:nl]))
		out = out[nl+1:]
		if len(header) == 2 && header[1] == "missing" {
			return nil, fmt.Errorf("git: object %s is missing", header[0])
		}
		if len(header) != 3 {
			return nil, fmt.Errorf("cat-file: bad header %q", header)
		}
		size, err := strconv.Atoi(header[2])
		if err != nil || len(out) < size+1 {
			return nil, fmt.Errorf("cat-file: truncated object %s", header[0])
		}
		res[header[0]] = append([]byte(nil), out[:size]...)
		out = out[size+1:]
	}
	return res, nil
}

// ShowFile returns the content of path at rev, or ok=false if it is absent.
func (g *Git) ShowFile(ctx context.Context, rev, path string) ([]byte, bool, error) {
	out, err := g.runRaw(ctx, nil, "ls-tree", "-z", "--full-tree", rev, "--", path)
	if err != nil {
		return nil, false, err
	}
	recs := splitNUL(out)
	if len(recs) == 0 {
		return nil, false, nil
	}
	meta, _, _ := strings.Cut(recs[0], "\t")
	f := strings.Fields(meta)
	if len(f) != 3 || f[1] != "blob" {
		return nil, false, nil
	}
	blobs, err := g.ReadBlobs(ctx, []string{f[2]})
	if err != nil {
		return nil, false, err
	}
	return blobs[f[2]], true, nil
}

// DiffNames lists paths that differ between two commits or trees, sorted.
func (g *Git) DiffNames(ctx context.Context, a, b string) ([]string, error) {
	out, err := g.runRaw(ctx, nil, "diff-tree", "-r", "-z", "--no-renames", "--name-only", a, b)
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}
