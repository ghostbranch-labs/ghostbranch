// Package contract defines the machine-readable surface of every command:
// exit codes, stable error and warning codes, and the JSON envelope. It is
// the single source their reference docs will be generated from.
package contract

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SchemaVersion versions the envelope and every result and event schema. It
// stays 0.x until launch, and 0.x versions may break.
const SchemaVersion = "0.14"

// RecordVersion versions what Ghostbranch stores: feature records, plans,
// grants, waivers, workspace.json and the stored login. It moves only when a
// record's format changes, so a binary one contract version older keeps
// working on records a newer one wrote (6.4a changed only the contract).
const RecordVersion = "0.12"

// Exit codes. Agents branch on these, so they never change meaning.
const (
	ExitOK            = 0
	ExitFailure       = 1
	ExitUsage         = 2
	ExitGateFailed    = 3
	ExitConflict      = 4
	ExitNeedsApproval = 5
	ExitPlanStale     = 6
)

// Code is a stable error code. Adding a code is compatible; renaming or
// removing one is a breaking change.
type Code string

const (
	AuthMissing      Code = "AUTH_MISSING"
	ForgeError       Code = "FORGE_ERROR"
	ShadowNotPrivate Code = "SHADOW_NOT_PRIVATE"
	ConfigInvalid    Code = "CONFIG_INVALID"
	Locked           Code = "LOCKED"
	Internal         Code = "INTERNAL"

	TokenCannotPublish Code = "TOKEN_CANNOT_PUBLISH"
	GuardUnsupported   Code = "GUARD_UNSUPPORTED"
	PushBlocked        Code = "PUSH_BLOCKED"
	// NothingToPublish means every feature commit is empty once overlay
	// paths and internal sections are stripped.
	NothingToPublish Code = "NOTHING_TO_PUBLISH"
	// AuthorUnmapped means promote.author_email is map and a commit's author
	// email is not in promote.author_map.
	AuthorUnmapped Code = "AUTHOR_UNMAPPED"

	BadFlag         Code = "BAD_FLAG"
	MissingInput    Code = "MISSING_INPUT"
	PairAmbiguous   Code = "PAIR_AMBIGUOUS"
	NameInvalid     Code = "NAME_INVALID"
	FeatureNotFound Code = "FEATURE_NOT_FOUND"
	// WaiverUnknown means waivers add named an id that is not a waivable
	// finding, or waivers revoke named an id with no stored waiver for the
	// repo.
	WaiverUnknown Code = "WAIVER_UNKNOWN"
	// CurateInvalid means a --curate file is malformed, or its groups do not
	// name every feature commit exactly once.
	CurateInvalid Code = "CURATE_INVALID"

	NotAPairCheckout Code = "NOT_A_PAIR_CHECKOUT"

	ChecksFailed Code = "CHECKS_FAILED"
	NoCI         Code = "NO_CI"
	CITimeout    Code = "CI_TIMEOUT"
	LeakFound    Code = "LEAK_FOUND"
	// MarkerInvalid means BEGIN-INTERNAL/END-INTERNAL markers could not be
	// stripped safely; the gate stops before CI.
	MarkerInvalid Code = "MARKER_INVALID"

	RebaseConflict   Code = "REBASE_CONFLICT"
	SyncDiverged     Code = "SYNC_DIVERGED"
	BranchExists     Code = "BRANCH_EXISTS"
	OverlayCollision Code = "OVERLAY_COLLISION"
	ShadowNotEmpty   Code = "SHADOW_NOT_EMPTY"
	RefMoved         Code = "REF_MOVED"
	// PushRefused means the remote refused a push for a reason of its own
	// (token permission, a ruleset, a hook); running it again is refused again.
	PushRefused Code = "PUSH_REFUSED"
	// ShadowCreateDenied means the token may not create the shadow repo; a
	// person creates it and init adopts it.
	ShadowCreateDenied Code = "SHADOW_CREATE_DENIED"
	// CurateConflict means replaying the curated groups in their order hit
	// a conflict; Ghostbranch never resolves it.
	CurateConflict Code = "CURATE_CONFLICT"
	// CurateTreeMismatch means the curated replay ends on a tree other than
	// the one the gate's CI tested.
	CurateTreeMismatch Code = "CURATE_TREE_MISMATCH"

	ApprovalRequired Code = "APPROVAL_REQUIRED"

	PlanStale Code = "PLAN_STALE"

	// UpgradeNeeded means the pair's overlay config uses keys this binary
	// doesn't know: the pair was set up by a newer Ghostbranch.
	UpgradeNeeded Code = "UPGRADE_NEEDED"

	// FeatureMoved means a feature's branch or refs moved while it was
	// being closed; nothing more was deleted.
	FeatureMoved Code = "FEATURE_MOVED"

	// ShadowBehindError means feature start refused because shadow main is
	// behind public main. It shares its string with the ShadowBehind
	// warning: one is an error.code, the other a warnings[].code.
	ShadowBehindError Code = "SHADOW_BEHIND"

	// Interrupted means the command was cancelled (Ctrl-C); details.job names
	// a gate job still pending.
	Interrupted Code = "INTERRUPTED"
	// JobNotFound means a feature has no gate job to wait on.
	JobNotFound Code = "JOB_NOT_FOUND"
	// JobStale means a newer gate replaced the job.
	JobStale Code = "JOB_STALE"
	// LoginExpired means the stored login's refresh token expired or was
	// revoked, so it can't be refreshed; a person logs in again.
	LoginExpired Code = "LOGIN_EXPIRED"
	// LoginUnavailable means login can't run or store a login here (no
	// console for the device flow, no OS keychain, no client ID in this
	// build, the flow was denied or its code expired), or the stored login
	// can't be read. details.reason says which.
	LoginUnavailable Code = "LOGIN_UNAVAILABLE"
	// TokenPermissionMissing means GitHub refused a call because the token
	// lacks a permission GitHub named. details.permission, details.repo and
	// details.command say which, where and for what.
	TokenPermissionMissing Code = "TOKEN_PERMISSION_MISSING"
	// DoctorFailed means at least one doctor check failed; details.failed
	// lists their IDs.
	DoctorFailed Code = "DOCTOR_FAILED"
	// FoldNeedsRebase means feature fold can't fast-forward the parent to the
	// feature: the parent moved on since the feature was branched from it.
	// details: feature, parent, forked_at, parent_head.
	FoldNeedsRebase Code = "FOLD_NEEDS_REBASE"
	// HasChildren means abandon or close refused a feature with children
	// stacked on it. details: feature, children.
	HasChildren Code = "HAS_CHILDREN"
	// NotAStack means promote's features aren't a stack it can publish
	// together. details.reason is duplicate, order, gap, parent_not_public,
	// parent_merged, parent_moved, stale_gate, open_above or parent_stale; details.feature names the
	// feature it is about.
	NotAStack Code = "NOT_A_STACK"
	// ParentRewritten means a feature's parent no longer contains forked_at,
	// the commit the feature was branched from. details: feature, parent,
	// forked_at.
	ParentRewritten Code = "PARENT_REWRITTEN"
	// PRChangedOnGitHub means promote refused to update an open PR because
	// its public branch holds commits Ghostbranch didn't publish: updating
	// would drop them. It shares its string with the PublicBranchChanged
	// warning: one is an error.code, the other a warnings[].code.
	// details.foreign lists each commit as {sha, author, subject};
	// details.feature and details.branch name where.
	PRChangedOnGitHub Code = "PUBLIC_BRANCH_CHANGED"
	// PairNotFound means the repo named (--repo, the checkout's remotes, or
	// a plan) isn't set up in this workspace and GHOSTBRANCH_HOME.
	// details.public names it when it can.
	PairNotFound Code = "PAIR_NOT_FOUND"
	// NoOpenPR means feature pull found no open public PR for the feature.
	// details: feature, reason (not_published, merged, closed, branch_gone),
	// public_pr once it was published.
	NoOpenPR Code = "NO_OPEN_PR"
	// PlanNotFound means no plan file with a well-formed ID is on this
	// machine. details: plan, reason (other_machine, unknown), public and
	// feature when known.
	PlanNotFound Code = "PLAN_NOT_FOUND"
)

var exitCodes = map[Code]int{
	AuthMissing: ExitFailure, ShadowCreateDenied: ExitFailure, ForgeError: ExitFailure, ShadowNotPrivate: ExitFailure,
	ConfigInvalid: ExitFailure, Locked: ExitFailure, Internal: ExitFailure,
	TokenCannotPublish: ExitFailure, GuardUnsupported: ExitFailure, PushBlocked: ExitFailure, PushRefused: ExitFailure,
	AuthorUnmapped: ExitFailure, NothingToPublish: ExitFailure,
	NotAPairCheckout: ExitUsage,
	BadFlag:          ExitUsage, MissingInput: ExitUsage, PairAmbiguous: ExitUsage,
	NameInvalid: ExitUsage, FeatureNotFound: ExitUsage,
	ChecksFailed: ExitGateFailed, NoCI: ExitGateFailed, CITimeout: ExitGateFailed, LeakFound: ExitGateFailed,
	MarkerInvalid: ExitGateFailed, WaiverUnknown: ExitUsage, CurateInvalid: ExitUsage,
	RebaseConflict: ExitConflict, SyncDiverged: ExitConflict, BranchExists: ExitConflict,
	OverlayCollision: ExitConflict, ShadowNotEmpty: ExitConflict, RefMoved: ExitConflict,
	CurateConflict: ExitConflict, CurateTreeMismatch: ExitConflict,
	ApprovalRequired:       ExitNeedsApproval,
	PlanStale:              ExitPlanStale,
	UpgradeNeeded:          ExitFailure,
	FeatureMoved:           ExitConflict,
	ShadowBehindError:      ExitConflict,
	Interrupted:            ExitFailure,
	JobNotFound:            ExitUsage,
	JobStale:               ExitConflict,
	LoginExpired:           ExitFailure,
	LoginUnavailable:       ExitFailure,
	TokenPermissionMissing: ExitFailure,
	DoctorFailed:           ExitFailure,
	FoldNeedsRebase:        ExitConflict,
	HasChildren:            ExitConflict,
	NotAStack:              ExitUsage,
	ParentRewritten:        ExitConflict,
	PRChangedOnGitHub:      ExitConflict,
	PairNotFound:           ExitUsage,
	NoOpenPR:               ExitUsage,
	PlanNotFound:           ExitUsage,
}

// Exit is the process exit code for c.
func (c Code) Exit() int {
	if e, ok := exitCodes[c]; ok {
		return e
	}
	return ExitFailure
}

// AllCodes lists every error code, sorted.
func AllCodes() []Code {
	out := make([]Code, 0, len(exitCodes))
	for c := range exitCodes {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Error is a failure with a stable code. Message, Field and Details are
// shown to callers; Err is the underlying cause and is never serialized.
// Messages must never contain secret values.
type Error struct {
	Code      Code
	Message   string
	Field     string
	Retryable bool
	Details   map[string]any
	Err       error
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }
func (e *Error) Unwrap() error { return e.Err }

// New makes an error with a formatted message.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap makes an error whose message ends with err's text.
func Wrap(code Code, err error, format string, args ...any) *Error {
	msg := fmt.Sprintf(format, args...)
	if err != nil {
		msg += ": " + err.Error()
	}
	return &Error{Code: code, Message: msg, Err: err}
}

// WithField names the input the error is about.
func (e *Error) WithField(field string) *Error { e.Field = field; return e }

// WithDetails attaches machine-readable details.
func (e *Error) WithDetails(d map[string]any) *Error { e.Details = d; return e }

// From returns err as a contract error. Errors without a code are INTERNAL.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var ce *Error
	if errors.As(err, &ce) {
		return ce
	}
	return &Error{Code: Internal, Message: err.Error(), Err: err}
}

// WarningCode is a stable warning code.
type WarningCode string

const (
	ConfigKeyIgnored WarningCode = "CONFIG_KEY_IGNORED"
	PublicNotPublic  WarningCode = "PUBLIC_NOT_PUBLIC"
	// GateNotRecorded means the gate's result could not be saved to the
	// feature metadata; the result itself still stands. With details.job the
	// job's record is still there for wait to finish.
	GateNotRecorded WarningCode = "GATE_NOT_RECORDED"
	// ChecksLimited means the token cannot read check runs, so the gate saw
	// only GitHub Actions workflows and commit statuses.
	ChecksLimited WarningCode = "CHECKS_LIMITED"
	// ActorUnknown means GHOSTBRANCH_ACTOR was unset and the GitHub login
	// could not be read, so the ledger records the actor as human:unknown.
	ActorUnknown WarningCode = "ACTOR_UNKNOWN"
	// WorkflowChanged, LargeFile and BinaryFile flag new or changed files
	// going public that a person should look at. They never block.
	WorkflowChanged WarningCode = "WORKFLOW_CHANGED"
	LargeFile       WarningCode = "LARGE_FILE"
	BinaryFile      WarningCode = "BINARY_FILE"
	// WaiversUnreadable means waivers.json could not be read, so no waiver
	// applied and every finding blocked.
	WaiversUnreadable WarningCode = "WAIVERS_UNREADABLE"
	// OverlayOutdated means a Ghostbranch-managed overlay file is missing,
	// out of date or edited by hand; ghostbranch upgrade fixes it.
	OverlayOutdated WarningCode = "OVERLAY_OUTDATED"
	// RepoGuard and RepoGuardUnrewritable flag workflow jobs or steps guarded
	// to the public repo's name, which skip in the shadow repo.
	RepoGuard             WarningCode = "REPO_GUARD"
	RepoGuardUnrewritable WarningCode = "REPO_GUARD_UNREWRITABLE"
	// SecretsUnlisted and SecretsMissing come from the secrets audit.
	SecretsUnlisted WarningCode = "SECRETS_UNLISTED"
	SecretsMissing  WarningCode = "SECRETS_MISSING"
	// PushTriggersOnSync names workflows a token-authenticated sync starts.
	PushTriggersOnSync WarningCode = "PUSH_TRIGGERS_ON_SYNC"
	// ActionsMinutesBilled: the shadow repo is private, so Actions is billed.
	ActionsMinutesBilled WarningCode = "ACTIONS_MINUTES_BILLED"
	// ShadowBehind means shadow main is behind public main.
	ShadowBehind WarningCode = "SHADOW_BEHIND"
	// AuditIncomplete means init or upgrade succeeded but the audit after it
	// could not run.
	AuditIncomplete WarningCode = "AUDIT_INCOMPLETE"
	// CINotCancelled means a sync's pushes may have started workflow runs
	// in the shadow repo that sync could not cancel.
	CINotCancelled WarningCode = "CI_NOT_CANCELLED"
	// PRStateUnknown means the survey couldn't read a published feature's
	// public PR, so it doesn't know whether the PR merged.
	PRStateUnknown WarningCode = "PR_STATE_UNKNOWN"
	// DriftUnknown means the survey couldn't check whether a feature still
	// merges cleanly onto public main.
	DriftUnknown WarningCode = "DRIFT_UNKNOWN"
	// FeatureNotClosed means sync didn't close a merged feature: its close
	// failed (details.feature names it), or sync couldn't survey the features
	// and closed none (no details.feature). The sync itself still succeeded.
	FeatureNotClosed WarningCode = "FEATURE_NOT_CLOSED"
	// MirrorNotUpdated means a push to the shadow repo succeeded, but the
	// local mirror's copy of a ref (or a local plan file) couldn't be
	// updated; the next fetch corrects the mirror.
	MirrorNotUpdated WarningCode = "MIRROR_NOT_UPDATED"
	// RecordNewer means a record was written by a newer Ghostbranch; this
	// binary shows what it can read and changes nothing in it.
	RecordNewer WarningCode = "RECORD_NEWER"
	// RecordUnreadable means a feature record couldn't be read; a person at
	// the console closed the feature anyway.
	RecordUnreadable WarningCode = "RECORD_UNREADABLE"
	// LedgerNotWritten means a command did what its result says, but its
	// ledger entry couldn't be written; after a close, details.refs keeps the
	// deleted refs and their SHAs.
	LedgerNotWritten WarningCode = "LEDGER_NOT_WRITTEN"
	// SyncPushFailed means sync couldn't push one of sync.branches to the
	// shadow repo; it still pushed the others.
	SyncPushFailed WarningCode = "SYNC_PUSH_FAILED"
	// PlanFileUnreadable means a plan file on this machine couldn't be read
	// and was skipped.
	PlanFileUnreadable WarningCode = "PLAN_FILE_UNREADABLE"
	// AgentOnPersonLogin means a run that looks like an agent or a script is
	// using a person's stored login instead of a token of its own.
	AgentOnPersonLogin WarningCode = "AGENT_ON_PERSON_LOGIN"
	// PublicBranchChanged means a published feature's public branch holds commits Ghostbranch didn't publish, so sync didn't adopt them; feature pull brings them in.
	PublicBranchChanged WarningCode = "PUBLIC_BRANCH_CHANGED"
	// PRTextNotSet means apply finished recording an update whose push had
	// already landed, but didn't set the PR title or body the plan types: no
	// one approved them.
	PRTextNotSet WarningCode = "PR_TEXT_NOT_SET"
	// UpdateAvailable means a newer Ghostbranch release is out. details:
	// current, latest, url, security, advisories, upgrade. It never changes
	// ok or exit_code.
	UpdateAvailable WarningCode = "UPDATE_AVAILABLE"
)

var warningCodes = []WarningCode{ConfigKeyIgnored, PublicNotPublic, GateNotRecorded, ChecksLimited, ActorUnknown,
	WorkflowChanged, LargeFile, BinaryFile, WaiversUnreadable,
	OverlayOutdated, RepoGuard, RepoGuardUnrewritable, SecretsUnlisted, SecretsMissing, PushTriggersOnSync,
	ActionsMinutesBilled, ShadowBehind, AuditIncomplete, CINotCancelled, PRStateUnknown, DriftUnknown, FeatureNotClosed,
	MirrorNotUpdated, RecordNewer, RecordUnreadable, LedgerNotWritten, SyncPushFailed, PlanFileUnreadable, AgentOnPersonLogin,
	PublicBranchChanged, PRTextNotSet, UpdateAvailable}

// AllWarnings lists every warning code, sorted.
func AllWarnings() []WarningCode {
	out := append([]WarningCode(nil), warningCodes...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Warning is a non-fatal notice returned alongside a result.
type Warning struct {
	Code    WarningCode    `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// Newer reports whether a record's schema version v (major.minor) is newer
// than the records this binary writes (RecordVersion). A malformed or empty v is not newer:
// records from before versions existed stay readable.
func Newer(v string) bool {
	// Only ASCII digits: Atoi alone takes a sign, so "+1.0" would be newer
	// and lock users out.
	digits := func(s string) bool {
		return s != "" && strings.Trim(s, "0123456789") == ""
	}
	parse := func(s string) (int, int, bool) {
		a, b, ok := strings.Cut(s, ".")
		if !ok || !digits(a) || !digits(b) {
			return 0, 0, false
		}
		x, err1 := strconv.Atoi(a)
		y, err2 := strconv.Atoi(b)
		return x, y, err1 == nil && err2 == nil
	}
	x, y, ok := parse(v)
	cx, cy, _ := parse(RecordVersion)
	return ok && (x > cx || (x == cx && y > cy))
}

// NewerRecord refuses to change a record written by a newer Ghostbranch.
// kind names the record: feature, plan, grants or waivers.
func NewerRecord(kind, version string) *Error {
	return New(UpgradeNeeded,
		"this %s record was written by a newer Ghostbranch (schema %s; this binary knows %s); upgrade ghostbranch before changing it",
		kind, version, RecordVersion).
		WithDetails(map[string]any{"record": kind, "record_version": version, "supported": RecordVersion})
}
