// Package managed holds the overlay files Ghostbranch writes and keeps up to
// date (today only the opt-in sync workflow), and decides whether the copy
// on an overlay branch is current.
package managed

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

//go:embed sync.yml.tmpl
var syncTemplate string

//go:embed sync.sh
var syncScript string

const (
	// SyncPath is the opt-in scheduled sync workflow.
	SyncPath    = ".github/workflows/ghostbranch-sync.yml"
	syncName    = "sync"
	SyncVersion = 1
)

// Input is the config a render depends on. Schedule "off" means scheduled
// sync is not wanted, so the sync workflow must not exist.
type Input struct {
	Public   string
	Schedule string
	Branches []string
}

type Status string

const (
	Current  Status = "current"
	Missing  Status = "missing"
	Outdated Status = "outdated"
	Modified Status = "modified"
	// Newer: a newer binary wrote it; this one leaves it alone.
	Newer Status = "newer"
)

// Paths lists every managed path, wanted or not.
func Paths() []string { return []string{SyncPath} }

// Script is the embedded sync script.
func Script() string { return syncScript }

// Describe names the managed templates and versions, for commit messages.
func Describe() string { return fmt.Sprintf("%s v%d", syncName, SyncVersion) }

func hashOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Render returns the bytes the config wants at each managed path. A path
// missing from the map must not exist on the overlay branch.
func Render(in Input) map[string][]byte {
	out := map[string][]byte{}
	if in.Schedule == "" || in.Schedule == "off" {
		return out
	}
	var script strings.Builder
	for _, line := range strings.SplitAfter(strings.TrimRight(syncScript, "\n")+"\n", "\n") {
		if strings.TrimSpace(line) == "" {
			script.WriteString(strings.TrimLeft(line, " \t"))
			continue
		}
		script.WriteString("          " + line)
	}
	body := strings.NewReplacer(
		"{{SCHEDULE}}", in.Schedule,
		"{{PUBLIC}}", in.Public,
		"{{BRANCHES}}", strings.Join(in.Branches, " "),
		"{{SCRIPT}}\n", script.String(),
	).Replace(syncTemplate)
	head := fmt.Sprintf("# ghostbranch-managed: %s v%d sha256=%s\n", syncName, SyncVersion, hashOf([]byte(body)))
	out[SyncPath] = []byte(head + body)
	return out
}

var header = regexp.MustCompile(`^# ghostbranch-managed: ([a-z-]+) v([0-9]+) sha256=([0-9a-f]{64})\n`)

// parse returns the header's name and version, and ok only when the header
// is present and the body still matches its hash.
func parse(b []byte) (string, int, bool) {
	m := header.FindSubmatch(b)
	if m == nil {
		return "", 0, false
	}
	v, err := strconv.Atoi(string(m[2]))
	if err != nil || hashOf(b[len(m[0]):]) != string(m[3]) {
		return "", 0, false
	}
	return string(m[1]), v, true
}

// StatusOf classifies the copy of path on the overlay branch.
func StatusOf(path string, cur []byte, exists bool, in Input) Status {
	want, wanted := Render(in)[path]
	if !exists {
		if wanted {
			return Missing
		}
		return Current
	}
	name, ver, ok := parse(cur)
	if !ok || path != SyncPath || name != syncName {
		return Modified
	}
	if ver > SyncVersion {
		return Newer
	}
	if !wanted || ver < SyncVersion || !bytes.Equal(cur, want) {
		return Outdated
	}
	return Current
}
