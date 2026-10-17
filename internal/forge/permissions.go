package forge

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

// The permissions doctor asks about, named as GitHub's fine-grained token
// settings name them.
const (
	PermMetadataRead   = "Metadata: read"
	PermContentsRead   = "Contents: read"
	PermContentsWrite  = "Contents: write"
	PermPullsRead      = "Pull requests: read"
	PermPullsWrite     = "Pull requests: write"
	PermActionsRead    = "Actions: read"
	PermActionsWrite   = "Actions: write"
	PermStatusesRead   = "Commit statuses: read"
	PermWorkflowsWrite = "Workflows: write"
	PermAdminWrite     = "Administration: write"
	PermSecretsRead    = "Secrets: read"
)

// What a probe learned about one permission.
const (
	AccessYes     = "yes"
	AccessNo      = "no"
	AccessUnknown = "unknown"
)

// RepoPermissions is what read-only probes learned about the token on one
// repo. Have maps a permission to AccessYes or AccessNo; a permission it
// doesn't list is unknown. Accepted holds GitHub's
// X-Accepted-GitHub-Permissions header from each refused probe, by
// permission.
type RepoPermissions struct {
	Have     map[string]string
	Accepted map[string]string
}

// PermissionProber is an optional interface for forges that can tell,
// without writing anything, what the token may do.
type PermissionProber interface {
	// ProbePermissions makes only GET requests (and the push probe, which
	// pushes nothing) against full. A refusal is an answer, not an error;
	// a network failure on the first request is an error wrapping
	// ErrTransient, and a later one leaves that permission unknown.
	ProbePermissions(ctx context.Context, full string) (RepoPermissions, error)
	// OAuthScopes is the token's classic OAuth scopes, from GET /user's
	// X-OAuth-Scopes header. ok is false when GitHub sent no such header
	// (fine-grained and App tokens).
	OAuthScopes(ctx context.Context) (scopes []string, ok bool, err error)
}

// AgentTokenPermissions are an agent's shadow-only token (the guide's
// Strict isolation): what feature start and gate need, and nothing that can
// publish. PersonTokenPermissions are a person's token for every command,
// init's shadow creation included.
var (
	AgentTokenPermissions  = []string{PermContentsWrite, PermPullsWrite, PermWorkflowsWrite, PermActionsRead, PermStatusesRead}
	PersonTokenPermissions = []string{PermContentsWrite, PermPullsWrite, PermWorkflowsWrite, PermActionsRead, PermStatusesRead, PermAdminWrite}
)

// tokenFormParams are the query parameters of GitHub's fine-grained token
// form, by the permission's name above, in the order TokenLink writes them.
// All six names (contents, pull_requests, workflows, actions, statuses and
// administration) were checked on GitHub's real form on 2026-10-17: each
// pre-filled the permission it names. Metadata (always read) and Secrets
// have none here.
var tokenFormParams = []struct{ perm, param string }{
	{"Contents", "contents"},
	{"Pull requests", "pull_requests"},
	{"Workflows", "workflows"},
	{"Actions", "actions"},
	{"Commit statuses", "statuses"},
	{"Administration", "administration"},
}

// TokenLink is GitHub's new fine-grained token page, pre-filled with name,
// description, the resource owner, an expiry in days and perms (a write
// wins over a read of the same permission). GitHub can't pre-fill the
// repository selection, so wherever the link is shown, also say: choose
// "Only select repositories", then pick the repos. A value with <...> (a
// placeholder, for the guide) keeps its angle brackets; GitHub names never
// contain them.
func TokenLink(name, description, owner string, days int, perms []string) string {
	level := map[string]string{}
	for _, p := range perms {
		base, lv, ok := strings.Cut(p, ": ")
		if !ok || level[base] == "write" {
			continue
		}
		level[base] = lv
	}
	esc := func(s string) string {
		return strings.NewReplacer("%3C", "<", "%3E", ">").Replace(url.QueryEscape(s))
	}
	var b strings.Builder
	b.WriteString("https://github.com/settings/personal-access-tokens/new?name=" + esc(name))
	if description != "" {
		b.WriteString("&description=" + esc(description))
	}
	b.WriteString("&target_name=" + esc(owner) + "&expires_in=" + strconv.Itoa(days))
	for _, f := range tokenFormParams {
		if lv := level[f.perm]; lv != "" {
			b.WriteString("&" + f.param + "=" + lv)
		}
	}
	return b.String()
}

// PairTokenLink is TokenLink for a person's token on a pair: every
// permission the commands need, on the shadow repo's owner, for 90 days.
// repos are the ones to pick under "Only select repositories": both, when
// one owner holds them (a token has one resource owner), else the shadow.
func PairTokenLink(public, shadow string) (link, repos string) {
	owner, _, _ := SplitName(shadow)
	pubOwner, name, _ := SplitName(public)
	repos, desc := public+" and "+shadow, "Ghostbranch: "+public+" and "+shadow
	if !strings.EqualFold(owner, pubOwner) {
		repos, desc = shadow, "Ghostbranch: "+shadow
	}
	return TokenLink("ghostbranch-"+name, desc, owner, 90, PersonTokenPermissions), repos
}
