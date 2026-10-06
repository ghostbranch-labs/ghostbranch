package contract

import (
	"strconv"
	"strings"
)

// Kind is what a registry entry describes.
type Kind string

const (
	KindExit    Kind = "exit"
	KindError   Kind = "error"
	KindWarning Kind = "warning"
)

// Doc explains one code to people and agents. It is the single source for
// `ghostbranch explain`, the agent guide's tables, and the user docs later.
type Doc struct {
	Summary    string
	Causes     []string
	WhatToDo   string
	NeedsHuman bool
}

// Entry is one registry row as `ghostbranch explain --json` prints it.
type Entry struct {
	Code       string   `json:"code"`
	Kind       Kind     `json:"kind"`
	ExitCode   *int     `json:"exit_code"`
	Summary    string   `json:"summary"`
	Causes     []string `json:"causes"`
	WhatToDo   string   `json:"what_to_do"`
	NeedsHuman bool     `json:"needs_human"`
}

var exitDocs = map[int]Doc{
	ExitOK:      {Summary: "Success.", WhatToDo: "Read data and next."},
	ExitFailure: {Summary: "The command failed for a reason other than usage, the gate, a conflict or approval.", WhatToDo: "Read error.code and run ghostbranch explain <code>; retry when error.retryable is true."},
	ExitUsage:   {Summary: "The command was called wrongly: a bad flag, missing input, or an unknown name.", WhatToDo: "Fix the input named in error.field and run the command again."},
	ExitGateFailed: {Summary: "The gate failed: CI failed or did not run, or the leak scan found something.",
		WhatToDo: "Fix the feature (code, tests or leaked content) and gate again. Never try to work around the leak scan."},
	ExitConflict: {Summary: "The repos are in a state the command will not change on its own: a rebase conflict, a diverged branch, or a name already taken.",
		WhatToDo: "Resolve the conflict named in the error, then run the command again."},
	ExitNeedsApproval: {Summary: "A person has to approve this before anything is published.",
		WhatToDo: "Stop. Give the human the command in next; it needs a person at a console, or an agent grant they make with ghostbranch agents allow.", NeedsHuman: true},
	ExitPlanStale: {Summary: "The repos changed since the plan was made, so the plan was not applied.",
		WhatToDo: "Make a new plan (the command in next) and apply that one."},
}

var errorDocs = map[Code]Doc{
	AuthMissing: {Summary: "No GitHub token was found.", Causes: []string{"GHOSTBRANCH_TOKEN, GH_TOKEN and GITHUB_TOKEN are unset and `gh auth token` gave nothing"},
		WhatToDo: "A person sets GHOSTBRANCH_TOKEN or runs `gh auth login`.", NeedsHuman: true},
	ForgeError: {Summary: "A GitHub API or git transport call failed.", Causes: []string{"network trouble", "GitHub outage or rate limit", "a token without the needed permission"},
		WhatToDo: "Retry when retryable is true; otherwise read the message for the failing call."},
	ShadowNotPrivate: {Summary: "The shadow repo is public or a fork, so Ghostbranch refuses to touch it.", Causes: []string{"someone changed the shadow repo's visibility"},
		WhatToDo: "A person makes the shadow repo private (and not a fork) on GitHub.", NeedsHuman: true},
	ConfigInvalid: {Summary: ".ghostbranch.yml on the overlay branch is missing or invalid, or the overlay branch holds non-overlay paths.",
		WhatToDo: "A person fixes .ghostbranch.yml on the shadow repo's ghostbranch branch.", NeedsHuman: true},
	Locked:   {Summary: "Another ghostbranch operation on this pair is running on this machine.", WhatToDo: "Wait and retry. If none is running, delete the lock file named in the message."},
	Internal: {Summary: "An unexpected local failure.", WhatToDo: "Read the message; report it if it repeats."},
	TokenCannotPublish: {Summary: "The token in use cannot push to the public repo.", Causes: []string{"the token is scoped to the shadow repo only (the strict-isolation setup)", "the token lacks Contents: write on the public repo"},
		WhatToDo: "A person publishes from their own terminal with a token that can write to the public repo (`ghostbranch apply <plan>`).", NeedsHuman: true},
	GuardUnsupported: {Summary: "The pre-push guard cannot be installed here.", Causes: []string{"git is older than 2.31", "the hooks directory is inside the working tree and not git-ignored, so the hook could be committed",
		"the checkout's hooks are managed by Husky", "a leftover pre-push.ghostbranch-chained file without a pre-push hook, or both files already exist"},
		WhatToDo: "Upgrade git, or point core.hooksPath at a directory that is ignored or outside the working tree."},
	PushBlocked: {Summary: "The pre-push guard refused to push feature work to the public repo.", Causes: []string{"pushing a feature branch, the overlay branch or a gate branch to the public remote", "pushing commits that contain a shadow feature"},
		WhatToDo: "Publish features only with ghostbranch promote. Never bypass the hook with --no-verify."},
	BadFlag:          {Summary: "A flag, argument or option value is invalid.", WhatToDo: "Fix the value named in error.field."},
	MissingInput:     {Summary: "Required input is missing.", WhatToDo: "Pass the input named in error.field, for example -m for promote."},
	PairAmbiguous:    {Summary: "Ghostbranch cannot tell which repo pair to use.", WhatToDo: "Pass --repo owner/name, or run init first."},
	NameInvalid:      {Summary: "A feature or branch name is invalid or reserved.", WhatToDo: "Choose another name; main, HEAD and names starting with ghostbranch are reserved."},
	FeatureNotFound:  {Summary: "No such feature in the shadow repo.", WhatToDo: "Check the name, or start it with ghostbranch feature start."},
	NotAPairCheckout: {Summary: "This directory is not a checkout of a registered public repo.", WhatToDo: "Run the command inside a clone of the public repo, after ghostbranch init."},
	ChecksFailed:     {Summary: "CI failed on the gate candidate.", WhatToDo: "Fix the failing checks listed in details.failed, push to the feature branch, and gate again."},
	NoCI:             {Summary: "No CI checks appeared on the gate candidate.", Causes: []string{"Actions is disabled in the shadow repo", "no workflow triggers on pull_request or push"}, WhatToDo: "Enable Actions or add a workflow, then gate again."},
	CITimeout:        {Summary: "CI did not finish in time.", WhatToDo: "Gate again, or raise gate.ci_timeout in .ghostbranch.yml."},
	LeakFound:        {Summary: "The leak scan found something in what would be published.", WhatToDo: "Remove the leaked content from the feature (see details.findings) and gate again. Agents never waive findings."},
	RebaseConflict:   {Summary: "The feature does not rebase cleanly onto public main.", WhatToDo: "Rebase the feature branch onto public main yourself, resolve the files in details.files, push, and gate again."},
	SyncDiverged:     {Summary: "Shadow main has commits that public main does not.", WhatToDo: "A person investigates; Ghostbranch never force-pushes shadow main.", NeedsHuman: true},
	BranchExists:     {Summary: "The name is already taken in the shadow or public repo.", WhatToDo: "Use another feature name, or promote with --branch <other-name>."},
	OverlayCollision: {Summary: "Public main already has paths Ghostbranch reserves for the overlay.", WhatToDo: "A person moves those paths out of public main.", NeedsHuman: true},
	ShadowNotEmpty:   {Summary: "The repo given to init --adopt has unrelated content.", WhatToDo: "A person picks an empty repo or a pure mirror of the public repo.", NeedsHuman: true},
	RefMoved:         {Summary: "A ref changed on the remote while the command ran.", WhatToDo: "Run the command again."},
	ApprovalRequired: {Summary: "Publishing needs a person: a code typed at a console, or an agent grant.", Causes: []string{"no_console: no terminal a person can type into", "not_confirmed, timeout, too_many_attempts: the person did not confirm", "grant_needs_yes: a grant exists, but --yes was not passed"},
		WhatToDo: "Stop and give the human the command in next. Never fake a terminal, answer the code, edit grants.json or run ghostbranch agents allow yourself.", NeedsHuman: true},
	PlanStale: {Summary: "The repos changed since the plan was made.", WhatToDo: "Make a new plan with ghostbranch promote <feature> --plan and apply that."},
}

var warningDocs = map[WarningCode]Doc{
	ConfigKeyIgnored: {Summary: ".ghostbranch.yml has a key this version does not use.", WhatToDo: "Nothing is required; remove the key to silence the warning."},
	PublicNotPublic:  {Summary: "The public repo is private.", WhatToDo: "Ghostbranch is meant for public repos; check you named the right repo."},
	GateNotRecorded:  {Summary: "The gate result could not be saved to the feature metadata; the result itself stands.", WhatToDo: "Gate again before promoting."},
	ChecksLimited:    {Summary: "The token cannot read check runs, so only GitHub Actions workflows and commit statuses were considered.", WhatToDo: "Use a token that can read checks if you rely on CI apps other than Actions."},
	ActorUnknown:     {Summary: "GHOSTBRANCH_ACTOR is unset and the GitHub login could not be read; the ledger records human:unknown.", WhatToDo: "Set GHOSTBRANCH_ACTOR, for example agent:claude-code."},
}

func intp(n int) *int { return &n }

func entry(code string, kind Kind, exit *int, d Doc) Entry {
	causes := d.Causes
	if causes == nil {
		causes = []string{}
	}
	return Entry{Code: code, Kind: kind, ExitCode: exit, Summary: d.Summary, Causes: causes, WhatToDo: d.WhatToDo, NeedsHuman: d.NeedsHuman}
}

// Registry lists every exit code (0-6), then every error code, then every
// warning code, each group sorted.
func Registry() []Entry {
	var out []Entry
	for x := ExitOK; x <= ExitPlanStale; x++ {
		out = append(out, entry(strconv.Itoa(x), KindExit, intp(x), exitDocs[x]))
	}
	for _, c := range AllCodes() {
		out = append(out, entry(string(c), KindError, intp(c.Exit()), errorDocs[c]))
	}
	for _, w := range AllWarnings() {
		out = append(out, entry(string(w), KindWarning, nil, warningDocs[w]))
	}
	return out
}

// Explain returns the entries for one code (any case) or one exit number.
// An exit number returns its own entry, then every error code mapped to it.
func Explain(key string) ([]Entry, error) {
	key = strings.TrimSpace(key)
	if n, err := strconv.Atoi(key); err == nil {
		d, ok := exitDocs[n]
		if !ok {
			return nil, New(BadFlag, "no exit code %d; exit codes are 0 to 6", n).WithField("code")
		}
		out := []Entry{entry(strconv.Itoa(n), KindExit, intp(n), d)}
		for _, c := range AllCodes() {
			if c.Exit() == n {
				out = append(out, entry(string(c), KindError, intp(n), errorDocs[c]))
			}
		}
		return out, nil
	}
	up := strings.ToUpper(key)
	if d, ok := errorDocs[Code(up)]; ok {
		return []Entry{entry(up, KindError, intp(Code(up).Exit()), d)}, nil
	}
	if d, ok := warningDocs[WarningCode(up)]; ok {
		return []Entry{entry(up, KindWarning, nil, d)}, nil
	}
	return nil, New(BadFlag, "unknown code %q; run ghostbranch explain --all", key).WithField("code")
}
