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
const SchemaVersion = "0.2"

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

	NotAPairCheckout Code = "NOT_A_PAIR_CHECKOUT"

	ChecksFailed Code = "CHECKS_FAILED"
	NoCI         Code = "NO_CI"
	CITimeout    Code = "CI_TIMEOUT"
	LeakFound    Code = "LEAK_FOUND"

	RebaseConflict   Code = "REBASE_CONFLICT"
	SyncDiverged     Code = "SYNC_DIVERGED"
	BranchExists     Code = "BRANCH_EXISTS"
	OverlayCollision Code = "OVERLAY_COLLISION"
	ShadowNotEmpty   Code = "SHADOW_NOT_EMPTY"
	RefMoved         Code = "REF_MOVED"

	ApprovalRequired Code = "APPROVAL_REQUIRED"

	PlanStale Code = "PLAN_STALE"
)

var exitCodes = map[Code]int{
	AuthMissing: ExitFailure, ForgeError: ExitFailure, ShadowNotPrivate: ExitFailure,
	ConfigInvalid: ExitFailure, Locked: ExitFailure, Internal: ExitFailure,
	TokenCannotPublish: ExitFailure, GuardUnsupported: ExitFailure, PushBlocked: ExitFailure,
	NotAPairCheckout: ExitUsage,
	BadFlag:          ExitUsage, MissingInput: ExitUsage, PairAmbiguous: ExitUsage,
	NameInvalid: ExitUsage, FeatureNotFound: ExitUsage,
	ChecksFailed: ExitGateFailed, NoCI: ExitGateFailed, CITimeout: ExitGateFailed, LeakFound: ExitGateFailed,
	RebaseConflict: ExitConflict, SyncDiverged: ExitConflict, BranchExists: ExitConflict,
	OverlayCollision: ExitConflict, ShadowNotEmpty: ExitConflict, RefMoved: ExitConflict,
	ApprovalRequired: ExitNeedsApproval,
	PlanStale:        ExitPlanStale,
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
)

var warningCodes = []WarningCode{ConfigKeyIgnored, PublicNotPublic, GateNotRecorded, ChecksLimited, ActorUnknown}

// AllWarnings lists every warning code, sorted.
func AllWarnings() []WarningCode {
	out := append([]WarningCode(nil), warningCodes...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Warning is a non-fatal notice returned alongside a result.
type Warning struct {
	Code    WarningCode `json:"code"`
	Message string      `json:"message"`
}
