// Package preview is the typed "what would change" list that --plan and
// --dry-run return instead of acting (spec §9.2). Agents read the ops;
// people read Describe.
package preview

import (
	"fmt"
	"time"
)

// Op is one operation. Keys are fixed per "op" value; see
// schemas/v0/preview.schema.json.
type Op map[string]any

type Preview struct {
	Fingerprint map[string]string `json:"fingerprint"`
	Ops         []Op              `json:"ops"`
}

// New starts a preview against the given repo state (nil for none).
func New(fingerprint map[string]string) *Preview {
	if fingerprint == nil {
		fingerprint = map[string]string{}
	}
	return &Preview{Fingerprint: fingerprint, Ops: []Op{}}
}

func (p *Preview) Add(ops ...Op) { p.Ops = append(p.Ops, ops...) }

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func CreateRepo(repo string) Op { return Op{"op": "create_repo", "repo": repo, "private": true} }

// PushRef is a ref update; "" for from means the ref is created, "" for to
// means its new value is only known once earlier steps run.
func PushRef(repo, ref, from, to string) Op {
	return Op{"op": "push_ref", "repo": repo, "ref": ref, "from": nullable(from), "to": nullable(to)}
}
func SetDefaultBranch(repo, branch string) Op {
	return Op{"op": "set_default_branch", "repo": repo, "branch": branch}
}
func OpenPR(repo, head, base string, draft bool) Op {
	return Op{"op": "open_pr", "repo": repo, "head": head, "base": base, "draft": draft}
}
func WaitChecks(repo, sha string) Op { return Op{"op": "wait_checks", "repo": repo, "sha": sha} }
func RunGate(feature string) Op      { return Op{"op": "run_gate", "feature": feature} }
func RegisterPair(public, shadow string) Op {
	return Op{"op": "register_pair", "public": public, "shadow": shadow}
}
func Grant(public, shadow string, expiresAt *time.Time) Op {
	var exp any
	if expiresAt != nil {
		exp = expiresAt.UTC().Format(time.RFC3339)
	}
	return Op{"op": "grant", "public": public, "shadow": shadow, "expires_at": exp}
}
func Revoke(public, shadow string) Op { return Op{"op": "revoke", "public": public, "shadow": shadow} }
func WriteFile(path string) Op        { return Op{"op": "write_file", "path": path} }
func RemoveFile(path string) Op       { return Op{"op": "remove_file", "path": path} }
func MoveFile(from, to string) Op     { return Op{"op": "move_file", "from": from, "to": to} }

// DeleteRef deletes ref from repo, leased on sha: the delete fails if the
// ref no longer points there.
func DeleteRef(repo, ref, sha string) Op {
	return Op{"op": "delete_ref", "repo": repo, "ref": ref, "sha": sha}
}

// EditPR sets the title and/or body (whichever is true) of open PR number
// in repo; the other stays as it is on GitHub.
func EditPR(repo string, number int, title, body bool) Op {
	return Op{"op": "edit_pr", "repo": repo, "number": number, "title": title, "body": body}
}

// ClosePR closes PR number in repo without merging it.
func ClosePR(repo string, number int) Op { return Op{"op": "close_pr", "repo": repo, "number": number} }

// OverlayFile is a change to a managed file on the overlay branch.
func OverlayFile(repo, path, action string) Op {
	return Op{"op": "overlay_file", "repo": repo, "path": path, "action": action}
}

func short(v any) string {
	s, _ := v.(string)
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// Describe renders each op as one line for people.
func (p *Preview) Describe() []string {
	out := make([]string, 0, len(p.Ops))
	for _, o := range p.Ops {
		var line string
		switch o["op"] {
		case "create_repo":
			line = fmt.Sprintf("create private repo %s", o["repo"])
		case "push_ref":
			switch {
			case o["from"] == nil && o["to"] == nil:
				line = fmt.Sprintf("create %s in %s", o["ref"], o["repo"])
			case o["from"] == nil:
				line = fmt.Sprintf("create %s in %s at %s", o["ref"], o["repo"], short(o["to"]))
			case o["to"] == nil:
				line = fmt.Sprintf("update %s in %s from %s", o["ref"], o["repo"], short(o["from"]))
			default:
				line = fmt.Sprintf("move %s in %s from %s to %s", o["ref"], o["repo"], short(o["from"]), short(o["to"]))
			}
		case "set_default_branch":
			line = fmt.Sprintf("make %s the default branch of %s", o["branch"], o["repo"])
		case "open_pr":
			kind := "PR"
			if o["draft"] == true {
				kind = "draft PR"
			}
			line = fmt.Sprintf("open a %s in %s from %s into %s", kind, o["repo"], o["head"], o["base"])
		case "delete_ref":
			line = fmt.Sprintf("delete %s from %s (at %s)", o["ref"], o["repo"], short(o["sha"]))
		case "close_pr":
			line = fmt.Sprintf("close PR #%v in %s", o["number"], o["repo"])
		case "edit_pr":
			what := "title and body"
			switch {
			case o["title"] != true:
				what = "body"
			case o["body"] != true:
				what = "title"
			}
			line = fmt.Sprintf("set PR #%v's %s in %s", o["number"], what, o["repo"])
		case "wait_checks":
			line = fmt.Sprintf("wait for CI on %s in %s", short(o["sha"]), o["repo"])
		case "run_gate":
			line = fmt.Sprintf("run the gate for %s (it is missing or stale), then plan again", o["feature"])
		case "register_pair":
			line = fmt.Sprintf("register %s with shadow %s in this workspace", o["public"], o["shadow"])
		case "grant":
			until := "until revoked"
			if o["expires_at"] != nil {
				until = fmt.Sprintf("until %s", o["expires_at"])
			}
			line = fmt.Sprintf("allow agents to publish to %s %s", o["public"], until)
		case "revoke":
			line = fmt.Sprintf("revoke the agent grant for %s", o["public"])
		case "waive":
			line = fmt.Sprintf("waive leak finding %s for %s", o["id"], o["public"])
		case "unwaive":
			line = fmt.Sprintf("revoke the waiver for leak finding %s on %s", o["id"], o["public"])
		case "write_file":
			line = fmt.Sprintf("write %s", o["path"])
		case "remove_file":
			line = fmt.Sprintf("remove %s", o["path"])
		case "move_file":
			line = fmt.Sprintf("move %s to %s", o["from"], o["to"])
		case "overlay_file":
			line = fmt.Sprintf("%s %s on the %s branch of %s", o["action"], o["path"], "ghostbranch", o["repo"])
		default:
			line = fmt.Sprintf("%v", o)
		}
		out = append(out, line)
	}
	return out
}

func Waive(public, id string) Op   { return Op{"op": "waive", "public": public, "id": id} }
func Unwaive(public, id string) Op { return Op{"op": "unwaive", "public": public, "id": id} }
