package cli

import (
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

func (a *app) editor(initial string) (string, error) {
	f, err := os.CreateTemp("", "ghostbranch-message-*.txt")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.WriteString(initial); err != nil {
		_ = f.Close()
		return "", err
	}
	_ = f.Close()
	ed := ""
	for _, k := range []string{"GIT_EDITOR", "VISUAL", "EDITOR"} {
		if ed = strings.TrimSpace(a.env.Getenv(k)); ed != "" {
			break
		}
	}
	if ed == "" {
		ed = "vi"
		if runtime.GOOS == "windows" {
			ed = "notepad"
		}
	}
	parts := splitCommand(ed, func(p string) bool { fi, err := os.Stat(p); return err == nil && !fi.IsDir() })
	if len(parts) == 0 {
		return "", contract.New(contract.MissingInput, "no editor configured: set GIT_EDITOR, VISUAL or EDITOR, or pass -m").WithField("message")
	}
	cmd := exec.Command(parts[0], append(parts[1:], f.Name())...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", contract.Wrap(contract.MissingInput, err, "editor %q failed", ed).WithField("message")
	}
	b, err := os.ReadFile(f.Name())
	return string(b), err
}

// splitCommand turns an editor setting into argv. A value that names an
// existing file is used whole (Windows paths often contain spaces);
// otherwise it splits on whitespace outside single or double quotes.
func splitCommand(s string, exists func(string) bool) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if exists(s) {
		return []string{s}
	}
	var out []string
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote == 0 && (r == '"' || r == '\''):
			quote, inWord = r, true
		case quote == 0 && (r == ' ' || r == '\t'):
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// cwdRemotes lists the checkout's remotes with origin first (git lists them
// alphabetically).
func cwdRemotes() []workspace.Remote {
	out, err := exec.Command("git", "remote", "-v").Output()
	if err != nil {
		return nil
	}
	var rs []workspace.Remote
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && !slices.ContainsFunc(rs, func(r workspace.Remote) bool { return r.Name == f[0] && r.URL == f[1] }) {
			rs = append(rs, workspace.Remote{Name: f[0], URL: f[1]})
		}
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Name == "origin" && rs[j].Name != "origin" })
	return rs
}
