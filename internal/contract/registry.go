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
	ExitNeedsApproval: {Summary: "A person has to approve this before it runs: a publish, or a delete or unregister that can't be undone.",
		WhatToDo: "Stop. Give the human the command in next; it needs a person at a console. For promote and apply, an agent grant they make with ghostbranch agents allow also works.", NeedsHuman: true},
	ExitPlanStale: {Summary: "The repos changed since the plan was made, so the plan was not applied.",
		WhatToDo: "Make a new plan (the command in next) and apply that one."},
}

var errorDocs = map[Code]Doc{
	AuthMissing: {Summary: "No GitHub token was found.",
		Causes:     []string{"GHOSTBRANCH_TOKEN, GH_TOKEN and GITHUB_TOKEN are unset, there is no stored login (ghostbranch login), and `gh auth token` gave nothing"},
		WhatToDo:   "A person runs `ghostbranch login`, or sets GHOSTBRANCH_TOKEN. Agents and CI use a token of their own in GHOSTBRANCH_TOKEN; the permissions each command needs are in the token table of `ghostbranch guide`. `gh auth login` also works.",
		NeedsHuman: true},
	LoginExpired: {Summary: "The stored GitHub login has expired and can't be refreshed.",
		Causes: []string{"its refresh token is past its six months",
			"the login was revoked on GitHub, or the Ghostbranch app was uninstalled or its authorization removed"},
		WhatToDo:   "A person runs ghostbranch login again. An agent stops and hands this to the person; an agent with a token of its own sets GHOSTBRANCH_TOKEN, which never uses the stored login.",
		NeedsHuman: true},
	LoginUnavailable: {Summary: "Ghostbranch can't log in here, or can't read the stored login (error.details.reason says which).",
		Causes: []string{
			"no_console: no terminal a person can use for the device flow; ghostbranch login --with-token reads a pasted token from stdin instead",
			"no_keychain: this machine has no usable OS keychain (for example Linux without a Secret Service), and Ghostbranch never stores a token in a plain file",
			"no_client_id: this build of Ghostbranch has no GitHub App client ID",
			"denied, expired_code: the person denied the request on GitHub, or the code expired before they approved it",
			"corrupt: the stored login can't be read",
			"keychain_error: the OS keychain exists but refused (for example, it is locked)",
			"refused: GitHub refused the login, for example because the app has device flow turned off",
			"refresh_failed: GitHub refused to refresh the stored login for a reason other than its expiry",
		},
		WhatToDo:   "Read error.details.reason. For no_console or no_keychain, set GHOSTBRANCH_TOKEN to a token instead; agents and CI always do. For no_client_id, use ghostbranch login --with-token or GHOSTBRANCH_TOKEN. For denied or expired_code, run ghostbranch login again and approve the code on GitHub. For corrupt, a person runs ghostbranch logout, then ghostbranch login. For keychain_error, unlock the keychain or set GHOSTBRANCH_TOKEN. For refused or refresh_failed, run ghostbranch login again.",
		NeedsHuman: true},
	TokenPermissionMissing: {Summary: "The token in use lacks a GitHub permission this command needs: error.details.permission on error.details.repo.",
		Causes: []string{
			"a fine-grained token or the Ghostbranch app wasn't granted that permission, for example Workflows: write to change .github/workflows files",
			"the repo isn't in the token's or the app installation's repository access",
			"a classic or OAuth token lacks the scope named (OAuth scope: ...)",
		},
		WhatToDo:   "Run ghostbranch doctor: it lists every permission the token lacks, for each command. A person grants error.details.permission on error.details.repo (in the fine-grained token's settings, or the Ghostbranch app installation's repository access), then runs the command in error.details.command again. Never ask for wider access than doctor names.",
		NeedsHuman: true},
	DoctorFailed: {Summary: "ghostbranch doctor found failing checks; error.details.failed lists their IDs.",
		Causes:   []string{"one of tools, token, pair, guard, state or ci failed; data.checks[].message says why"},
		WhatToDo: "Run the fix each failing check names (data.checks[].fix, also listed in next), then run ghostbranch doctor again. Warnings don't fail doctor."},
	ForgeError: {Summary: "A GitHub API or git transport call failed.", Causes: []string{"network trouble", "GitHub outage or rate limit", "a token without the needed permission"},
		WhatToDo: "Retry when retryable is true; otherwise read the message for the failing call."},
	ShadowNotPrivate: {Summary: "The shadow repo is public or a fork, so Ghostbranch refuses to touch it.", Causes: []string{"someone changed the shadow repo's visibility"},
		WhatToDo: "A person makes the shadow repo private (and not a fork) on GitHub.", NeedsHuman: true},
	ConfigInvalid: {Summary: ".ghostbranch.yml on the overlay branch is missing or invalid, or the overlay branch holds non-overlay paths.",
		WhatToDo: "A person fixes .ghostbranch.yml on the shadow repo's ghostbranch branch.", NeedsHuman: true},
	Locked:   {Summary: "Another ghostbranch operation on this pair is running on this machine (error.details.held_by names it).", WhatToDo: "Wait for it to finish and retry. The lock frees itself when that process ends, even if it crashed; there is nothing to delete."},
	Internal: {Summary: "An unexpected local failure.", WhatToDo: "Read the message; report it if it repeats."},
	TokenCannotPublish: {Summary: "The token in use cannot push to the public repo.", Causes: []string{"the token is scoped to the shadow repo only (the strict-isolation setup)", "the token lacks Contents: write on the public repo"},
		WhatToDo: "A person publishes from their own terminal with a token that can write to the public repo (`ghostbranch apply <plan>`).", NeedsHuman: true},
	GuardUnsupported: {Summary: "The pre-push guard cannot be installed here.", Causes: []string{"git is older than 2.31", "the hooks directory is inside the working tree and the guard's files there (pre-push and pre-push.ghostbranch-*) are not all git-ignored, so they could be committed",
		"the checkout's hooks are managed by Husky", "a leftover pre-push.ghostbranch-chained file without a pre-push hook, or both files already exist"},
		WhatToDo: "Upgrade git, ignore pre-push and pre-push.ghostbranch-* in the hooks directory, or point core.hooksPath at a directory that is ignored or outside the working tree."},
	PushBlocked: {Summary: "The pre-push guard refused to push feature work to the public repo.", Causes: []string{"pushing a feature branch, the overlay branch or a gate branch to the public remote", "pushing commits that contain a shadow feature"},
		WhatToDo: "Publish features only with ghostbranch promote. Never bypass the hook with --no-verify."},
	BadFlag:          {Summary: "A flag, argument or option value is invalid.", WhatToDo: "Fix the value named in error.field."},
	MissingInput:     {Summary: "Required input is missing.", WhatToDo: "Pass the input named in error.field, for example --title or -m for promote."},
	PairAmbiguous:    {Summary: "Ghostbranch cannot tell which repo pair to use.", WhatToDo: "Pass --repo owner/name, or --repo . inside a checkout of the public repo. If the pair isn't registered on this machine: ghostbranch init <public> --adopt when its shadow repo already exists, otherwise ghostbranch init <public>."},
	NameInvalid:      {Summary: "A feature or branch name is invalid or reserved.", WhatToDo: "Choose another name; main, HEAD and names starting with ghostbranch are reserved."},
	FeatureNotFound:  {Summary: "No such feature in the shadow repo.", WhatToDo: "Check the name, or start it with ghostbranch feature start."},
	NotAPairCheckout: {Summary: "This directory is not a checkout of a registered public repo.", WhatToDo: "Run the command inside a clone of the public repo, after ghostbranch init."},
	ChecksFailed:     {Summary: "CI failed on the gate candidate.", WhatToDo: "Fix the failing checks listed in details.failed, push to the feature branch, and gate again."},
	NoCI:             {Summary: "No CI checks appeared on the gate candidate.", Causes: []string{"Actions is disabled in the shadow repo", "no workflow triggers on pull_request or push"}, WhatToDo: "Enable Actions or add a workflow, then gate again."},
	CITimeout:        {Summary: "CI did not finish in time.", WhatToDo: "Gate again, or raise gate.ci_timeout in .ghostbranch.yml."},
	LeakFound: {Summary: "The leak scan found something in what would be published.",
		WhatToDo: "Remove the content in details.findings from the feature and gate again. A finding whose commits are listed may be only in older commits: publish with --mode squash or --mode curated so they never ship, or rewrite those commits on the feature branch, force-push it to the shadow repo, and gate again. If a person judges a finding in details.waivable a false positive, they run the waivers add command in next. Agents never waive findings or edit waivers.json."},
	MarkerInvalid: {Summary: "A file has BEGIN-INTERNAL/END-INTERNAL markers that cannot be stripped safely.",
		Causes: []string{"unmatched_begin: a begin marker with no end", "unmatched_end: an end marker with no begin",
			"nested: a begin marker inside an open section", "malformed: a line that looks like a marker but is not exactly one (more text, a doubled prefix, or a multi-line comment)",
			"unsupported_encoding: markers in a UTF-16 or UTF-32 file, or in a file read as binary"},
		WhatToDo: "Fix the marker lines at details.problems on the feature branch, push it to the shadow repo, and gate again. When details.commit names an older commit and the final tree no longer has the problem, publish with --mode squash or --mode curated so it never ships, or rewrite that commit on the feature branch and force-push it to the shadow repo. Never delete only the marker lines: the internal lines would then be published."},
	WaiverUnknown: {Summary: "waivers add named an id that is not a waivable finding in any commit the feature would publish now (it scans every commit, as a preserve promote does; under a squash or curated default whose older commit has a broken marker, only the final tree), or waivers revoke named an id with no stored waiver for the repo.",
		Causes: []string{"the content changed, so the finding has a new id",
			"the finding was only in a commit that is no longer on the feature branch (waivers add scans every commit on it, whatever the default mode, except that under a squash or curated default whose older commit has a broken marker it scans only the final tree)",
			"the finding is in the commit message, branch or PR text, which is never waivable", "a typo",
			"for waivers revoke: the waiver was already revoked, or is for another repo"},
		WhatToDo: "For waivers add, gate again and use the ids in error.details.waivable. For waivers revoke, run ghostbranch waivers list --repo <public> and revoke the ids it shows; error.details.ids lists the ids with no waiver."},
	RebaseConflict:   {Summary: "The feature does not rebase cleanly onto public main.", WhatToDo: "Rebase the feature branch onto public main yourself, resolve the files in details.files (the commit in details.commit stopped), push, and gate again."},
	SyncDiverged:     {Summary: "A shadow branch has commits that the public branch does not.", WhatToDo: "A person investigates the branches in details.branches; Ghostbranch never force-pushes a shadow branch.", NeedsHuman: true},
	BranchExists:     {Summary: "The name is already taken in the shadow or public repo.", WhatToDo: "With error.details.plan_id the branch holds an earlier, interrupted publish of this feature: resume it with ghostbranch apply <plan_id> rather than promoting under another name. Otherwise use another feature name, or promote with --branch <other-name>."},
	OverlayCollision: {Summary: "Public main already has paths Ghostbranch reserves for the overlay.", WhatToDo: "A person moves those paths out of public main.", NeedsHuman: true},
	ShadowNotEmpty:   {Summary: "The repo given to init --adopt has unrelated content.", WhatToDo: "A person picks an empty repo or a pure mirror of the public repo.", NeedsHuman: true},
	RefMoved:         {Summary: "A ref changed on the remote while the command ran.", WhatToDo: "Run the command again."},
	PushRefused: {Summary: "The remote repo refused the push.",
		Causes: []string{"the token lacks a permission the push needs, for example Workflows write (fine-grained) or the workflow scope (gh/OAuth) to change .github/workflows files",
			"a repository ruleset or branch protection (GH013)", "a server-side hook"},
		WhatToDo:   "Read error.details.reason. Fix the cause (usually a token permission a person grants) and run the command again; retrying unchanged will be refused again.",
		NeedsHuman: true},
	CurateInvalid: {Summary: "The --curate file is malformed, or its groups don't name every feature commit exactly once.",
		Causes: []string{"not JSON of the form {\"commits\": [{\"from\": [<sha>...], \"message\": \"...\"}]}",
			"a group with no commits or an empty message", "a SHA that is missing, repeated, ambiguous, or not one of the feature's commits"},
		WhatToDo: "Fix the file using error.details (missing, repeated, unknown, ambiguous). The feature's commits are listed in the gate result's commits."},
	CurateConflict: {Summary: "Replaying the curated groups in their order hit a conflict.",
		WhatToDo: "Change the group order in the --curate file so commits that touch the same lines keep their original order, or reorder the commits on your feature branch yourself. Ghostbranch never resolves conflicts."},
	CurateTreeMismatch: {Summary: "The curated replay ends on a different tree from the one the gate's CI tested.",
		WhatToDo: "Keep commits that touch the same lines in their original order, or reorder on your feature branch, gate again, and use preserve."},
	AuthorUnmapped: {Summary: "promote.author_email is map, and some commit authors are not in promote.author_map.",
		WhatToDo:   "A person adds the emails in error.details.emails to promote.author_map in .ghostbranch.yml on the overlay branch, or switches author_email to noreply.",
		NeedsHuman: true},
	NothingToPublish: {Summary: "Nothing would be published: every feature commit is empty once overlay paths and internal sections are stripped.",
		WhatToDo: "Nothing to do; the feature has no public changes."},
	ShadowCreateDenied: {Summary: "The token may not create the shadow repo.",
		Causes: []string{"a fine-grained token can't name a repo that doesn't exist yet, so creating one needs Administration write on all of the owner's repositories",
			"the token lacks Administration write in the owner account or org"},
		WhatToDo:   "A person creates the empty private repo in error.details.shadow (no README, license or .gitignore), on GitHub's new-repository page or with `gh repo create <shadow> --private`, and adds it to the token's repository access (see the token table in ghostbranch guide). Then run the init --adopt command in next. Never ask for a token with wider access.",
		NeedsHuman: true},
	ApprovalRequired: {Summary: "This needs a person: a code typed at a console. For promote and apply, an agent grant also works.", Causes: []string{"no_console: no terminal a person can type into", "not_confirmed, timeout, too_many_attempts: the person did not confirm", "grant_needs_yes: a grant exists, but --yes was not passed"},
		WhatToDo: "Stop and give the human the command in next. It needs a person at a console; for promote and apply, an agent grant made with ghostbranch agents allow also works (feature close and remove ignore grants). Never fake a terminal, answer the code, edit grants.json or run ghostbranch agents allow yourself.", NeedsHuman: true},
	PlanStale: {Summary: "The repos changed since the plan was made.", WhatToDo: "Make a new plan with ghostbranch promote <feature> --plan and apply that."},
	UpgradeNeeded: {Summary: "Something this pair uses was written by a newer Ghostbranch: a record (error.details.record: feature, plan, grants or waivers) or the overlay config (error.details.unknown_key, a key this binary doesn't know).",
		WhatToDo: "Upgrade ghostbranch on this machine to the version that wrote it, or newer (the other machine is probably newer). Don't edit the record or the overlay config to make an older binary accept it. If the key in error.details.unknown_key is a typo, a person fixes it in .ghostbranch.yml on the overlay branch."},
	FeatureMoved: {Summary: "A feature's branch or refs moved while it was being closed, so the close stopped; nothing more was deleted.",
		WhatToDo: "Run ghostbranch feature show <name> to see the new state, then close it again if that's still wanted."},
	ShadowBehindError: {Summary: "Shadow main is behind public main, so feature start refused to start a feature from it; nothing was created.",
		WhatToDo: "Run ghostbranch sync --repo <public>, then start the feature again."},
	Interrupted: {Summary: "The command was interrupted (Ctrl-C) before it finished.",
		WhatToDo: "With error.details.plan_id the plan is saved and the publish may have started: resume it with ghostbranch apply <plan_id> (never promote again; a person runs it unless an agent grant exists). With error.details.job the gate's CI is still running in the shadow repo: run ghostbranch wait <job>. With neither, nothing is pending: run the command again."},
	JobNotFound: {Summary: "The feature has no gate job to wait on: it was never gated, or its last gate stopped before CI (a leak or a conflict).",
		WhatToDo: "Run ghostbranch gate <feature>, with --detach to get a job id."},
	JobStale: {Summary: "A newer gate replaced this job: the feature, public main or the overlay changed, and the feature was gated again.",
		WhatToDo: "Wait on the current job (error.details.current_job), or, if there is none, gate the feature again."},
	FoldNeedsRebase: {Summary: "feature fold only fast-forwards, and the parent has moved on since the feature was branched from it, so nothing was folded.",
		Causes: []string{"commits were pushed to the parent after the feature was started from it or last restacked onto it",
			"the parent was rewritten (rebased, amended or force-pushed)"},
		WhatToDo: "Bring the feature up to its parent's tip, then fold again. ghostbranch feature restack <parent> does it, and stops with git steps on a conflict. Or rebase it yourself (error.details.steps): git fetch shadow && git switch <feature> && git rebase --onto shadow/<parent> <forked_at> <feature>, resolve any conflicts, then git push --force-with-lease shadow <feature>. error.details.parent and error.details.forked_at name them. Ghostbranch never resolves conflicts."},
	HasChildren: {Summary: "The feature has children stacked on it (error.details.children), so it was not abandoned or closed: they would be left on a deleted parent.",
		WhatToDo: "Deal with each child in error.details.children first: fold it into this feature (ghostbranch feature fold <child>), abandon it (for abandon: ghostbranch feature abandon <child>) or close it (for close: ghostbranch feature close <child>), or move it onto main (ghostbranch feature restack <child> --onto main). An abandoned child blocks close, not abandon. Then run the command again."},
	NotAStack: {Summary: "The features given to promote aren't a stack that can be published together; error.details.reason says why and error.details.feature names where.",
		Causes: []string{
			"duplicate: a feature is listed twice",
			"order: the features aren't listed bottom first",
			"gap: a listed feature's parent is neither the feature listed just below it nor already public (for example promote a c, leaving out b)",
			"parent_not_public: the bottom feature's parent is neither main nor already published",
			"parent_merged: the bottom's published parent merged; run sync",
			"parent_moved: the bottom's published parent changed since it was published",
			"stale_gate: a listed feature has no fresh, green gate; a gate goes stale when its own branch or any branch below it moves",
		},
		WhatToDo: "Run ghostbranch status to see the stack. List the features bottom first, starting from one whose parent is main or already published, with none left out. Gate each one that needs it (ghostbranch gate <feature>, bottom up), then run promote again."},
	ParentRewritten: {Summary: "The feature's parent no longer contains the commit the feature was branched from (forked_at): the parent was rebased, amended or force-pushed.",
		WhatToDo: "Run ghostbranch feature restack <parent> to replay the feature's own commits onto the parent's new tip (it stops with git steps on a conflict), or ghostbranch feature restack <feature> --onto main to move it off the parent. Then run the command again. error.details.parent and error.details.forked_at name them."},
}

var warningDocs = map[WarningCode]Doc{
	ConfigKeyIgnored:  {Summary: ".ghostbranch.yml has a key this version does not use.", WhatToDo: "Nothing is required; remove the key to silence the warning."},
	PublicNotPublic:   {Summary: "The public repo is private.", WhatToDo: "Ghostbranch is meant for public repos; check you named the right repo."},
	GateNotRecorded:   {Summary: "The gate result could not be saved to the feature metadata; the result itself stands.", WhatToDo: "With details.job, run ghostbranch wait <job>: it records the outcome the job's CI gave. Without it (the gate stopped on a leak), fix the leak and gate again. promote needs a recorded green gate."},
	ChecksLimited:     {Summary: "The token cannot read check runs, so only GitHub Actions workflows and commit statuses were considered.", WhatToDo: "Use a token that can read checks if you rely on CI apps other than Actions."},
	ActorUnknown:      {Summary: "GHOSTBRANCH_ACTOR is unset and the GitHub login wasn't read; the ledger records human:unknown.", WhatToDo: "Set GHOSTBRANCH_ACTOR, for example agent:claude-code."},
	WorkflowChanged:   {Summary: "Workflow files under .github/workflows are new or changed in what would be published.", WhatToDo: "A person checks the workflow changes in details.paths before approving the publish."},
	LargeFile:         {Summary: "New or changed files are larger than leak_scan.large_file.", WhatToDo: "Check that the files in details.paths are meant to go public."},
	BinaryFile:        {Summary: "New or changed binary files would be published.", WhatToDo: "Check that the files in details.paths are meant to go public; they were scanned only through their printable text."},
	WaiversUnreadable: {Summary: "waivers.json could not be read, so no waiver applied and every finding blocked.", WhatToDo: "A person fixes the file or clears it with ghostbranch waivers revoke --all. If the message says a newer Ghostbranch wrote it, upgrade ghostbranch on this machine instead: revoke --all keeps a newer file.", NeedsHuman: true},
	OverlayOutdated: {Summary: "A Ghostbranch-managed file on the overlay branch is missing, out of date, or edited by hand.",
		WhatToDo: "Run ghostbranch upgrade --plan to see the change, then ghostbranch upgrade."},
	RepoGuard: {Summary: "A workflow job or step is guarded to the public repo's name, so it skips in the shadow repo (ci.rewrite_repo_guards is warn).",
		WhatToDo: "Set ci.rewrite_repo_guards: rewrite (the default) so the gate's CI runs it, or widen the guard in the public workflow (which writes the shadow repo's name into the public repo). Keep ci.rewrite_repo_guards: warn if a guarded job deploys or publishes: rewritten, it runs in the shadow repo on unreleased code, and its secrets must never be added there."},
	RepoGuardUnrewritable: {Summary: "A workflow guard mentions the public repo in a form the gate can't rewrite safely, so that job skips in the shadow repo and CI does not test it.",
		Causes:   []string{"startsWith, contains, fromJSON or format around github.repository", "a YAML form the rewrite can't re-emit safely, such as an anchor or a multi-line plain value"},
		WhatToDo: "A person widens the guard in the public workflow to also allow the shadow repo, for example github.repository == 'org/project' || github.repository == 'org/project-shadow'. That writes the shadow repo's name into the public repo. Never widen the guard of a job that deploys or publishes: it would run in the shadow repo on unreleased code, and its secrets must never be added there; repos whose guarded jobs deploy should use ci.rewrite_repo_guards: warn. Guards in run: scripts, reusable workflows called by repo name and composite actions are never detected."},
	SecretsUnlisted: {Summary: "The token can't list the shadow repo's secret names, so the audit could not tell which referenced secrets are missing.",
		WhatToDo: "Check that the names in data.secrets.referenced exist in the shadow repo, or give the token Secrets: read."},
	SecretsMissing: {Summary: "Workflows reference secrets the shadow repo doesn't have.",
		WhatToDo: "A person adds only the secrets the tests need. Never add deploy or publish credentials to the shadow repo: guarded jobs run there on unreleased code. Ghostbranch never reads or copies secret values.", NeedsHuman: true},
	PushTriggersOnSync: {Summary: "These workflows run on pushes to synced branches. ghostbranch sync pushes with your token, and so does a scheduled sync that uses GHOSTBRANCH_SYNC_TOKEN, so each sync starts them in the shadow repo, re-testing code public CI already tested, on private Actions minutes.",
		WhatToDo: "ghostbranch sync cancels the runs its pushes start when the token has Actions write on the shadow repo. To stop them for good, a person adds the guard in details.guard to those workflows' push-triggered jobs, for example if: github.event_name != 'push' || github.repository == 'org/project'. A job that already has an if: combines them: if: <existing> && (github.event_name != 'push' || github.repository == 'org/project'). Once every push-triggered job carries it, the audit stops reporting the file and sync stops looking for its runs. It changes nothing in the public repo's own CI. Or accept the minutes. A scheduled sync pushed with the workflow's default token starts no workflows."},
	ActionsMinutesBilled: {Summary: "The shadow repo is private, so its Actions minutes are billed to its owner.",
		WhatToDo: "Nothing is required. Scheduled sync bills at least one minute a run (every 6 hours is about 120 minutes a month); turn it off with sync.schedule: off and ghostbranch upgrade."},
	ShadowBehind: {Summary: "Shadow main is behind public main.",
		WhatToDo: "Run ghostbranch sync --repo <public> (fast-forward only). The gate is unaffected: it always rebases on public main."},
	AuditIncomplete: {Summary: "init or upgrade finished, but the CI audit after it could not run.",
		WhatToDo: "Run ghostbranch audit."},
	CINotCancelled: {Summary: "Sync pushed, but its pushes may have started workflow runs in the shadow repo that it could not cancel.",
		Causes:   []string{"the token lacks Actions write (or read) on the shadow repo", "GitHub's Actions API failed"},
		WhatToDo: "Nothing is required: the runs only cost Actions minutes; cancel them on the shadow repo's Actions tab if you like. To let sync cancel them, give the token Actions write on the shadow repo. To stop them starting, add the guard that PUSH_TRIGGERS_ON_SYNC in ghostbranch audit suggests. ghostbranch sync --no-cancel skips cancelling. details.runs lists the runs sync knows of, each with its id and url; it is empty when sync couldn't list them."},
	PRStateUnknown: {Summary: "Ghostbranch couldn't read a published feature's public PR state.",
		WhatToDo: "Run the command again; a feature whose PR state is unknown is never closed automatically."},
	DriftUnknown: {Summary: "Ghostbranch couldn't check whether a feature still merges cleanly onto public main.",
		WhatToDo: "Run ghostbranch gate <feature>; the gate is the authority on rebasing."},
	FeatureNotClosed: {Summary: "Sync didn't close a merged feature: the close failed, sync couldn't survey the features and so closed none, or another feature's record couldn't be read, so sync left the merged features open.",
		WhatToDo: "If details.deleted is set, the feature is gone: close its gate draft PR in the shadow repo by hand. With details.unreadable, the features named there have records that can't be read, and any of them may be stacked on this one: a person closes each at the console (ghostbranch feature close <name>) or fixes its record, and the next sync closes this feature. Otherwise run ghostbranch feature show <name>, then feature close <name>. With no details.feature, sync couldn't survey the features and closed none: run ghostbranch status to see why."},
	MirrorNotUpdated: {Summary: "The shadow repo has the change, but this machine's mirror of it (a ref, or a plan file) couldn't be updated.",
		Causes:   []string{"the mirror directory is not writable, or the disk is full", "a stale lock file in the mirror"},
		WhatToDo: "Nothing is required: the shadow repo is the truth, and the next command's fetch corrects the mirror. If it repeats, check the path in details and the disk."},
	RecordNewer: {Summary: "A record was written by a newer Ghostbranch; this binary shows what it can read of it and won't change it.",
		WhatToDo: "Upgrade ghostbranch on this machine (the other machine is probably newer). Reads still work; commands that would change the record stop with UPGRADE_NEEDED."},
	RecordUnreadable: {Summary: "A feature's record couldn't be read (corrupt, or from a newer Ghostbranch); a person at the console closed the feature anyway.",
		WhatToDo: "Nothing more to do: the feature's refs were deleted and logged in the ledger with their SHAs, so a branch can still be recovered."},
	LedgerNotWritten: {Summary: "The command did what its result says, but its entry in the ledger (ghostbranch log) couldn't be written.",
		WhatToDo: "Nothing is undone and nothing needs running again. Keep details.refs when it is there: after a close it lists every deleted ref with its SHA, which the ledger would have kept for recovering a branch. If it repeats, check that the workspace's ledger file is writable and the disk has room."},
	SyncPushFailed: {Summary: "Sync couldn't push one of sync.branches to the shadow repo; it still pushed the others, cancelled their runs and closed merged features.",
		WhatToDo: "details.branch is the branch and details.code the push's error code. For REF_MOVED, run ghostbranch sync again. For PUSH_REFUSED or a permission error, a person fixes the token or the shadow repo's rules, then runs sync again."},
	PlanFileUnreadable: {Summary: "A plan file on this machine couldn't be read, so it was skipped.",
		WhatToDo: "Look at details.path: fix its permissions or delete it. If it belonged to a feature that was just closed, that feature's plan ref may remain in the shadow repo; it is harmless."},
	AgentOnPersonLogin: {Summary: "This run looks like an agent or a script, and it is using a person's stored login, which can publish.",
		WhatToDo: "Give the agent its own shadow-only token in GHOSTBRANCH_TOKEN (see ghostbranch guide § Tokens). The person's login still needs the console code to publish."},
	PublicBranchChanged: {Summary: "A stacked feature's public branch holds commits Ghostbranch didn't publish, so sync didn't adopt them.",
		WhatToDo: "Look at the PR on GitHub: someone pushed to its branch. Ghostbranch never adopts a change it didn't scan; re-gate and promote again, or remove the foreign commits."},
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
// A string that names both an error and a warning (SHADOW_BEHIND) returns
// both entries, the error first.
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
	var out []Entry
	if d, ok := errorDocs[Code(up)]; ok {
		out = append(out, entry(up, KindError, intp(Code(up).Exit()), d))
	}
	if d, ok := warningDocs[WarningCode(up)]; ok {
		out = append(out, entry(up, KindWarning, nil, d))
	}
	if len(out) > 0 {
		return out, nil
	}
	return nil, New(BadFlag, "unknown code %q; run ghostbranch explain --all", key).WithField("code")
}
