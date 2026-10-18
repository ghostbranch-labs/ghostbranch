package engine_test

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge/fake"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

var tokenPair = workspace.Pair{Public: publicName, Shadow: shadowName}

// agentTokenForge is a fake forge on an offered agent token: both repos
// exist, and the token can't push to the public repo.
func agentTokenForge(t *testing.T) *fake.Forge {
	t.Helper()
	f := fake.New(filepath.Join(t.TempDir(), "forge"))
	for _, r := range []struct {
		full    string
		private bool
	}{{publicName, false}, {shadowName, true}} {
		if _, err := f.AddRepo(r.full, r.private); err != nil {
			t.Fatal(err)
		}
	}
	f.PushDenied = map[string]bool{publicName: true}
	return f
}

func refusalReason(t *testing.T, err error) *contract.Error {
	t.Helper()
	ce := wantCode(t, err, contract.AgentTokenRefused)
	if link, _ := ce.Details["link"].(string); link != engine.AgentTokenLink(tokenPair) {
		t.Errorf("details.link = %q, want the agent token link", link)
	}
	return ce
}

func TestCheckAgentTokenAccepted(t *testing.T) {
	t.Parallel()
	f := agentTokenForge(t)
	c, err := engine.CheckAgentToken(context.Background(), f, tokenPair)
	if err != nil {
		t.Fatal(err)
	}
	if c.Account != "octo" || c.Shadow != shadowName || c.CanPublish != "false" || len(c.Missing) != 0 {
		t.Errorf("check = %+v", c)
	}
	if err := engine.AgentTokenRefusal(c, tokenPair); err != nil {
		t.Errorf("a shadow-only token was refused: %v", err)
	}
}

func TestCheckAgentTokenCanPublish(t *testing.T) {
	t.Parallel()
	f := agentTokenForge(t)
	f.PushDenied = nil // the token reaches the public repo too
	c, err := engine.CheckAgentToken(context.Background(), f, tokenPair)
	if err != nil {
		t.Fatal(err)
	}
	if c.CanPublish != "true" {
		t.Fatalf("can_publish = %q, want true", c.CanPublish)
	}
	ce := refusalReason(t, engine.AgentTokenRefusal(c, tokenPair))
	if ce.Details["reason"] != "can_publish" || !strings.Contains(ce.Message, publicName) {
		t.Errorf("refusal = %v %v", ce.Message, ce.Details)
	}
}

// workflowsDenied is a forge whose probe says the token lacks Workflows:
// write on the shadow repo. GitHub can't be asked that without writing, so
// the fake has no switch for it.
type workflowsDenied struct{ *fake.Forge }

func (w workflowsDenied) ProbePermissions(ctx context.Context, full string) (forge.RepoPermissions, error) {
	p, err := w.Forge.ProbePermissions(ctx, full)
	if err == nil && full == shadowName {
		p.Have[forge.PermWorkflowsWrite] = forge.AccessNo
	}
	return p, err
}

func TestCheckAgentTokenMissingWorkflows(t *testing.T) {
	t.Parallel()
	f := workflowsDenied{agentTokenForge(t)}
	c, err := engine.CheckAgentToken(context.Background(), f, tokenPair)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Missing, []string{forge.PermWorkflowsWrite}) || c.CanPublish != "false" {
		t.Fatalf("check = %+v", c)
	}
	ce := refusalReason(t, engine.AgentTokenRefusal(c, tokenPair))
	if ce.Details["reason"] != "missing_permissions" || !slices.Equal(stringsOf(ce.Details["missing"]), []string{forge.PermWorkflowsWrite}) {
		t.Errorf("refusal = %v %v", ce.Message, ce.Details)
	}
}

func TestCheckAgentTokenMissingContentsWrite(t *testing.T) {
	t.Parallel()
	f := agentTokenForge(t)
	f.PushDenied[shadowName] = true // a read-only token on the shadow
	c, err := engine.CheckAgentToken(context.Background(), f, tokenPair)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Missing, []string{forge.PermContentsWrite}) {
		t.Fatalf("missing = %v", c.Missing)
	}
}

// A token that can't see the shadow repo is the wrong repo, even when it
// can also publish: that is the first thing to fix.
func TestCheckAgentTokenWrongRepoComesFirst(t *testing.T) {
	t.Parallel()
	f := fake.New(filepath.Join(t.TempDir(), "forge"))
	if _, err := f.AddRepo(publicName, false); err != nil {
		t.Fatal(err)
	}
	c, err := engine.CheckAgentToken(context.Background(), f, tokenPair)
	if err != nil {
		t.Fatal(err)
	}
	ce := refusalReason(t, engine.AgentTokenRefusal(c, tokenPair))
	if ce.Details["reason"] != "wrong_repo" || !strings.Contains(ce.Message, shadowName) {
		t.Errorf("refusal = %v %v", ce.Message, ce.Details)
	}
}

// pushUnknown is a forge whose push probe couldn't tell (a network
// failure, or an HTTP status other than 200 and 403).
type pushUnknown struct{ *fake.Forge }

func (pushUnknown) PushAccess(context.Context, string) forge.PushAccess {
	return forge.PushAccess{Can: "unknown", HTTPStatus: 502}
}

// A push probe that can't tell is not proof the token can't publish.
func TestCheckAgentTokenUnknownPushIsNotAccepted(t *testing.T) {
	t.Parallel()
	c, err := engine.CheckAgentToken(context.Background(), pushUnknown{agentTokenForge(t)}, tokenPair)
	if err != nil {
		t.Fatal(err)
	}
	if c.CanPublish != "unknown" {
		t.Fatalf("can_publish = %q", c.CanPublish)
	}
	err = engine.AgentTokenRefusal(c, tokenPair)
	if err == nil {
		t.Fatal("a token whose push access is unknown was accepted")
	}
	if ce := contract.From(err); ce.Code == contract.AgentTokenRefused || !ce.Retryable {
		t.Errorf("want a retryable error that isn't a refusal, got %v", ce)
	}
}

func TestCheckAgentTokenAccountError(t *testing.T) {
	t.Parallel()
	f := agentTokenForge(t)
	f.UserErr = forge.ErrForbidden
	if _, err := engine.CheckAgentToken(context.Background(), f, tokenPair); err == nil {
		t.Fatal("want an error when GitHub won't say whose token it is")
	}
}

func TestAgentTokenLinkAndSetLines(t *testing.T) {
	t.Parallel()
	want := forge.TokenLink("ghostbranch-agent-widget", "Ghostbranch agent: "+shadowName, "acme", 30, forge.AgentTokenPermissions)
	if got := engine.AgentTokenLink(tokenPair); got != want {
		t.Errorf("link = %s\nwant %s", got, want)
	}
	lines := engine.AgentTokenSetLines()
	if lines["sh"] != "export GHOSTBRANCH_TOKEN=<paste>" || lines["powershell"] != "$env:GHOSTBRANCH_TOKEN = '<paste>'" || len(lines) != 2 {
		t.Errorf("set lines = %v", lines)
	}
}
