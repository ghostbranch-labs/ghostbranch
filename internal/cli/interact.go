package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

func (a *app) prompt(summary string) (bool, error) {
	_, _ = fmt.Fprintf(a.env.Stderr, "%s\nProceed? [y/N] ", summary)
	line, err := bufio.NewReader(a.env.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

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
	parts := strings.Fields(ed)
	cmd := exec.Command(parts[0], append(parts[1:], f.Name())...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", contract.Wrap(contract.MissingInput, err, "editor %q failed", ed).WithField("message")
	}
	b, err := os.ReadFile(f.Name())
	return string(b), err
}

func cwdRemotes() []string {
	out, err := exec.Command("git", "remote", "-v").Output()
	if err != nil {
		return nil
	}
	var urls []string
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			urls = append(urls, f[1])
		}
	}
	return urls
}
