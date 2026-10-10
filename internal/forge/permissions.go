package forge

import "context"

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
