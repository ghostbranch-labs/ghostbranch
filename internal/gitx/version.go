package gitx

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Version is `git version`'s output, trimmed: "git version 2.47.1" and the
// like. It reads no repository.
func Version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "version").Output()
	if err != nil {
		return "", fmt.Errorf("run git version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
