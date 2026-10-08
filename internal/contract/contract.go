// Package contract defines the machine-readable surface of every command:
// exit codes, stable error and warning codes, and the JSON envelope. It is
// the single source their reference docs will be generated from.
package contract

import (
	"errors"
	"fmt"
	"sort"
)

// SchemaVersion versions the envelope and every data schema. It stays 0.x
// until launch, and 0.x versions may break.
const SchemaVersion = "0.6"

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

	BadFlag         Code = "BAD_FLAG"
	MissingInput    Code = "MISSING_INPUT"
	PairAmbiguous   Code = "PAIR_AMBIGUOUS"
	NameInvalid     Code = "NAME_INVALID"
	FeatureNotFound Code = "FEATURE_NOT_FOUND"
	// WaiverUnknown means waivers add named an id that is not a waivable
	// finding in what the gate would publish now.
	WaiverUnknown Code = "WAIVER_UNKNOWN"
	// CurateInvalid means a --curate file is malformed, or its groups do not
	// name every feature commit exactly once.
	CurateInvalid Code = "CURATE_INVALID"
	// NothingToPublish means every feature commit is empty once overlay
	// paths and internal sections are stripped.
	NothingToPublish Code = "NOTHING_TO_PUBLISH"
	// AuthorUnmapped means promote.author_email is map and a commit's author
	// email is not in promote.author_map.
	AuthorUnmapped Code = "AUTHOR_UNMAPPED"

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
	ApprovalRequired:  ExitNeedsApproval,
	PlanStale:         ExitPlanStale,
	UpgradeNeeded:     ExitFailure,
	FeatureMoved:      ExitConflict,
	ShadowBehindError: ExitConflict,
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
	// feature metadata; the result itself still stands.
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
	// FeatureNotClosed means sync's auto-close of a merged feature failed;
	// the sync itself still succeeded.
	FeatureNotClosed WarningCode = "FEATURE_NOT_CLOSED"
	// MirrorNotUpdated means a push to the shadow repo succeeded, but the
	// local mirror's copy of a ref (or a local plan file) couldn't be
	// updated; the next fetch corrects the mirror.
	MirrorNotUpdated WarningCode = "MIRROR_NOT_UPDATED"
)

var warningCodes = []WarningCode{ConfigKeyIgnored, PublicNotPublic, GateNotRecorded, ChecksLimited, ActorUnknown,
	WorkflowChanged, LargeFile, BinaryFile, WaiversUnreadable,
	OverlayOutdated, RepoGuard, RepoGuardUnrewritable, SecretsUnlisted, SecretsMissing, PushTriggersOnSync,
	ActionsMinutesBilled, ShadowBehind, AuditIncomplete, CINotCancelled, PRStateUnknown, DriftUnknown, FeatureNotClosed,
	MirrorNotUpdated}

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
