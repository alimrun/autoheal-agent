package recovery

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/alimrun/autoheal-agent/internal/api"
	"github.com/alimrun/autoheal-agent/internal/config"
)

var (
	taskIDPattern = regexp.MustCompile(`^[0-9A-Za-z]{26}$`)
	noncePattern  = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type payload struct {
	Protocol          int             `json:"protocol"`
	TaskID            string          `json:"task_id"`
	Nonce             string          `json:"nonce"`
	AgentID           string          `json:"agent_id"`
	EnvironmentID     string          `json:"environment_id"`
	Action            string          `json:"action"`
	Arguments         json.RawMessage `json:"arguments"`
	PolicyVersion     int             `json:"policy_version"`
	CatalogVersion    int             `json:"catalog_version"`
	VerificationCheck string          `json:"verification_check"`
	TimeoutSeconds    int             `json:"timeout_seconds"`
	IssuedAt          time.Time       `json:"issued_at"`
	ExpiresAt         time.Time       `json:"expires_at"`
}

type arguments struct {
	Target *string `json:"target"`
}

// Verifier decides whether an envelope may be acted on. It changes nothing: the caller records
// an accepted task in the replay store before reporting it received.
type Verifier struct {
	keys   map[string]ed25519.PublicKey
	policy Policy
	replay *ReplayStore
	now    func() time.Time
}

func NewVerifier(keys map[string]ed25519.PublicKey, policy Policy, replay *ReplayStore) *Verifier {
	return &Verifier{keys: keys, policy: policy, replay: replay, now: time.Now}
}

// Verify checks the signature over the exact payload bytes before reading them, then the
// protocol, time window, agent, environment, replay store, local policy and argument schema.
func (verifier *Verifier) Verify(envelope api.TaskEnvelope, identity Identity) (*Task, *Refusal) {
	signed, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return nil, &Refusal{Code: RefusalBadSignature}
	}
	refuse := refuser(signed)

	key, known := verifier.keys[envelope.KeyID]
	if !known {
		return nil, refuse(RefusalUnknownKey)
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil || !ed25519.Verify(key, signed, signature) {
		return nil, refuse(RefusalBadSignature)
	}

	var head struct {
		Protocol int `json:"protocol"`
	}
	if err := json.Unmarshal(signed, &head); err != nil || head.Protocol != Protocol {
		return nil, refuse(RefusalUnsupportedProtocol)
	}
	var parsed payload
	if err := decodeStrict(signed, &parsed); err != nil || !taskIDPattern.MatchString(parsed.TaskID) || !noncePattern.MatchString(parsed.Nonce) {
		return nil, refuse(RefusalUnsupportedProtocol)
	}

	now := verifier.now()
	switch {
	case now.After(parsed.ExpiresAt):
		return nil, refuse(RefusalExpired)
	case parsed.IssuedAt.After(now.Add(MaxSkew)):
		return nil, refuse(RefusalNotYetValid)
	case parsed.ExpiresAt.Sub(parsed.IssuedAt) > MaxLifetime:
		return nil, refuse(RefusalExpired)
	case parsed.AgentID != identity.AgentID:
		return nil, refuse(RefusalWrongAgent)
	case parsed.EnvironmentID != identity.EnvironmentID:
		return nil, refuse(RefusalWrongEnvironment)
	case verifier.replay.Seen(parsed.TaskID, parsed.Nonce):
		return nil, refuse(RefusalReplayed)
	case parsed.Action != ActionRecheckHealth && parsed.Action != ActionClearCache:
		return nil, refuse(RefusalUnknownAction)
	case !slices.Contains(verifier.policy.Actions(), parsed.Action):
		return nil, refuse(RefusalNotEnabled)
	}

	var given arguments
	if err := decodeStrict(parsed.Arguments, &given); err != nil || given.Target == nil || !config.TargetPattern.MatchString(*given.Target) {
		return nil, refuse(RefusalInvalidArguments)
	}
	if !verifier.enabled(parsed.Action, *given.Target) {
		return nil, refuse(RefusalNotEnabled)
	}

	return &Task{
		ID:                parsed.TaskID,
		Nonce:             parsed.Nonce,
		Action:            parsed.Action,
		Target:            *given.Target,
		VerificationCheck: parsed.VerificationCheck,
		Timeout:           time.Duration(parsed.TimeoutSeconds) * time.Second,
		ExpiresAt:         parsed.ExpiresAt,
	}, nil
}

func (verifier *Verifier) enabled(action, target string) bool {
	if action == ActionClearCache {
		_, declared := verifier.policy.hook(target)
		return declared
	}
	local, own := strings.CutPrefix(target, verifier.policy.Prefix+":")
	return own && slices.Contains(verifier.policy.Rechecks, local)
}

// refuser builds refusals for one payload. The task ID and nonce it carries are only what the
// payload claims: they route the report and are never acted on.
func refuser(signed []byte) func(code string) *Refusal {
	var claimed struct {
		TaskID string `json:"task_id"`
		Nonce  string `json:"nonce"`
	}
	_ = json.Unmarshal(signed, &claimed)
	if !taskIDPattern.MatchString(claimed.TaskID) || !noncePattern.MatchString(claimed.Nonce) {
		claimed.TaskID, claimed.Nonce = "", ""
	}
	return func(code string) *Refusal {
		return &Refusal{Code: code, TaskID: claimed.TaskID, Nonce: claimed.Nonce}
	}
}

// decodeStrict reads exactly one JSON object with no unknown fields and nothing after it.
func decodeStrict(content []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected content after the JSON object")
	}
	return nil
}
