// Package recovery verifies the signed recovery tasks AutoHeal sends (protocol 1) and runs the
// two actions an operator can allow: checking health again and calling a loopback cache hook.
// Nothing here can start a process.
package recovery

import (
	"slices"
	"time"
)

const (
	Protocol = 1

	ActionRecheckHealth = "recheck_health"
	ActionClearCache    = "clear_cache"

	// MaxSkew is how far ahead of this host's clock a task may be issued.
	MaxSkew = 2 * time.Minute
	// MaxLifetime is the longest a task may stay valid after it was issued.
	MaxLifetime = 10 * time.Minute
)

// Refusal codes, as the platform's TaskRefusal lists them.
const (
	RefusalBadSignature        = "bad_signature"
	RefusalUnknownKey          = "unknown_key"
	RefusalUnsupportedProtocol = "unsupported_protocol"
	RefusalExpired             = "expired"
	RefusalNotYetValid         = "not_yet_valid"
	RefusalWrongAgent          = "wrong_agent"
	RefusalWrongEnvironment    = "wrong_environment"
	RefusalReplayed            = "replayed"
	RefusalUnknownAction       = "unknown_action"
	RefusalInvalidArguments    = "invalid_arguments"
	RefusalNotEnabled          = "not_enabled"
	RefusalBusy                = "busy"
)

// Failure codes for an action that ran and did not finish.
const (
	FailureHookError  = "hook_error"
	FailureHookStatus = "hook_status"
	FailureTimeout    = "timeout"
	FailureCheckError = "check_error"
)

// Task is a verified task. Every field comes from the signed payload.
type Task struct {
	ID                string
	Nonce             string
	Action            string
	Target            string
	VerificationCheck string
	Timeout           time.Duration
	ExpiresAt         time.Time
}

// Refusal says why an envelope was not accepted. TaskID and Nonce are empty when the envelope
// gave none that can be reported; such a refusal is logged and dropped.
type Refusal struct {
	Code   string
	TaskID string
	Nonce  string
}

// Reportable reports whether the platform can be told about the refusal.
func (refusal *Refusal) Reportable() bool {
	return refusal.TaskID != "" && refusal.Nonce != ""
}

// Identity is this agent, as its stored credential names it.
type Identity struct {
	AgentID       string
	EnvironmentID string
}

// Policy is what the local configuration allows. Targets never come from the platform.
type Policy struct {
	// Prefix is this agent's host prefix, which starts every check name it reports.
	Prefix string
	// Rechecks are the local names of the checks recheck_health may run.
	Rechecks []string
	Hooks    []Hook
}

// Hook is one declared cache hook on this host.
type Hook struct {
	Name      string
	URL       string
	TokenFile string
	Timeout   time.Duration
}

// Actions lists the actions with at least one target, in protocol order.
func (policy Policy) Actions() []string {
	var actions []string
	if len(policy.Rechecks) > 0 {
		actions = append(actions, ActionRecheckHealth)
	}
	if len(policy.Hooks) > 0 {
		actions = append(actions, ActionClearCache)
	}
	return actions
}

func (policy Policy) hook(name string) (Hook, bool) {
	index := slices.IndexFunc(policy.Hooks, func(hook Hook) bool { return hook.Name == name })
	if index < 0 {
		return Hook{}, false
	}
	return policy.Hooks[index], true
}
