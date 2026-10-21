// Package skills holds the agent skill. The same file is published to skill
// directories and plugin marketplaces and embedded in the binary, so there is
// only one copy to keep up to date.
package skills

import _ "embed"

// Ghostbranch is skills/ghostbranch/SKILL.md.
//
//go:embed ghostbranch/SKILL.md
var Ghostbranch string
