package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
)

// The pre-filled token links come from forge's lists; doctor checks
// permGroups. They must say the same: the agent's token is exactly what
// gate needs on the shadow repo, and the person's has every permission
// any command needs (optional ones aside).
func TestTokenLinkPermissionsMatchDoctor(t *testing.T) {
	t.Parallel()
	var gate, all, wanted []string
	for _, g := range permGroups {
		for _, n := range g.needs {
			if n.optional {
				if g.name == "gate" {
					wanted = append(wanted, n.perm)
				}
				continue
			}
			if g.name == "gate" {
				gate = append(gate, n.perm)
			}
			all = append(all, n.perm)
		}
	}
	covers := func(list []string, p string) bool {
		if slices.Contains(list, p) {
			return true
		}
		base, ok := strings.CutSuffix(p, ": read")
		return ok && slices.Contains(list, base+": write")
	}
	for _, p := range gate {
		if !slices.Contains(forge.AgentTokenPermissions, p) {
			t.Errorf("gate needs %s; the agent token link lacks it", p)
		}
	}
	if len(gate) != len(forge.AgentTokenPermissions) {
		t.Errorf("the agent token link %v holds more than gate needs %v", forge.AgentTokenPermissions, gate)
	}
	for _, p := range all {
		if !covers(forge.PersonTokenPermissions, p) {
			t.Errorf("a command needs %s; the person's token link lacks it", p)
		}
	}
	// The links also ask for what gate wants but can do without.
	if !slices.Equal(forge.WantedTokenPermissions, wanted) {
		t.Errorf("the links want %v; gate's optional permissions are %v", forge.WantedTokenPermissions, wanted)
	}
}
