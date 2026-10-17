package forge_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
)

// TokenLink pre-fills GitHub's fine-grained token form: name, description,
// resource owner, expiry, then one parameter per permission, a write
// winning over a read of the same permission. Permissions the form can't
// pre-fill (Metadata, Secrets) are left out.
func TestTokenLink(t *testing.T) {
	t.Parallel()
	got := forge.TokenLink("ghostbranch-agent-widget", "Ghostbranch agent: acme/widget-shadow only", "acme", 30, []string{
		forge.PermMetadataRead, forge.PermContentsRead, forge.PermContentsWrite, forge.PermPullsWrite, forge.PermWorkflowsWrite,
		forge.PermActionsRead, forge.PermStatusesRead, forge.PermAdminWrite, forge.PermSecretsRead, forge.PermActionsWrite,
	})
	const want = "https://github.com/settings/personal-access-tokens/new?name=ghostbranch-agent-widget" +
		"&description=Ghostbranch+agent%3A+acme%2Fwidget-shadow+only&target_name=acme&expires_in=30" +
		"&contents=write&pull_requests=write&workflows=write&actions=write&statuses=read&administration=write"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if _, err := url.Parse(got); err != nil {
		t.Fatal(err)
	}
}

// A placeholder such as <repo> stays readable, for the guide.
func TestTokenLinkKeepsPlaceholders(t *testing.T) {
	t.Parallel()
	got := forge.TokenLink("ghostbranch-agent-<repo>", "", "<shadow-owner>", 30, forge.AgentTokenPermissions)
	if !strings.Contains(got, "name=ghostbranch-agent-<repo>&target_name=<shadow-owner>&expires_in=30&") {
		t.Fatalf("got %s", got)
	}
	if !strings.HasSuffix(got, "&contents=write&pull_requests=write&workflows=write&actions=read&statuses=read") {
		t.Fatalf("agent permissions: %s", got)
	}
}
