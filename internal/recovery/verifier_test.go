package recovery

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alimrun/autoheal-agent/internal/api"
)

const (
	testTaskID = "01JCTASK000000000000000001"
	testNonce  = "0123456789abcdef0123456789abcdef"
	testKeyID  = "k1"
)

var (
	testNow      = time.Date(2026, 11, 12, 12, 1, 0, 0, time.UTC)
	testIdentity = Identity{AgentID: "01JCAGENT00000000000000001", EnvironmentID: "01JCENV0000000000000000001"}
	testPolicy   = Policy{
		Prefix:   "web-01",
		Rechecks: []string{"api"},
		Hooks:    []Hook{{Name: "views", URL: "http://127.0.0.1:8080/cache/clear", Timeout: 30 * time.Second}},
	}
)

// signer stands in for the platform, with a key pair made for the test.
type signer struct {
	public  ed25519.PublicKey
	private ed25519.PrivateKey
}

func newSigner(t *testing.T) signer {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return signer{public: public, private: private}
}

func (platform signer) keys() map[string]ed25519.PublicKey {
	return map[string]ed25519.PublicKey{testKeyID: platform.public}
}

func (platform signer) sign(signed []byte) api.TaskEnvelope {
	return api.TaskEnvelope{
		KeyID:     testKeyID,
		Payload:   base64.StdEncoding.EncodeToString(signed),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(platform.private, signed)),
	}
}

func (platform signer) envelope(t *testing.T, change func(fields map[string]any)) api.TaskEnvelope {
	t.Helper()
	return platform.sign(payloadJSON(t, change))
}

// payloadJSON is a valid clear_cache payload for testIdentity at testNow, after change.
func payloadJSON(t *testing.T, change func(fields map[string]any)) []byte {
	t.Helper()
	fields := map[string]any{
		"protocol":           1,
		"task_id":            testTaskID,
		"nonce":              testNonce,
		"agent_id":           testIdentity.AgentID,
		"environment_id":     testIdentity.EnvironmentID,
		"action":             ActionClearCache,
		"arguments":          map[string]any{"target": "views"},
		"policy_version":     3,
		"catalog_version":    1,
		"verification_check": "web-01:api",
		"timeout_seconds":    60,
		"issued_at":          "2026-11-12T12:00:00Z",
		"expires_at":         "2026-11-12T12:05:00Z",
	}
	if change != nil {
		change(fields)
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func openStore(t *testing.T, stateDir string) *ReplayStore {
	t.Helper()
	store, err := OpenReplayStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newVerifier(t *testing.T, platform signer, policy Policy) *Verifier {
	t.Helper()
	verifier := NewVerifier(platform.keys(), policy, openStore(t, t.TempDir()))
	verifier.now = func() time.Time { return testNow }
	return verifier
}

func TestVerifyAcceptsBothActions(t *testing.T) {
	platform := newSigner(t)
	verifier := newVerifier(t, platform, testPolicy)

	task, refusal := verifier.Verify(platform.envelope(t, nil), testIdentity)
	if refusal != nil {
		t.Fatalf("clear_cache refused: %+v", refusal)
	}
	want := Task{
		ID: testTaskID, Nonce: testNonce, Action: ActionClearCache, Target: "views", VerificationCheck: "web-01:api",
		Timeout: time.Minute, ExpiresAt: time.Date(2026, 11, 12, 12, 5, 0, 0, time.UTC),
	}
	if *task != want {
		t.Errorf("task:\n got %+v\nwant %+v", *task, want)
	}

	task, refusal = verifier.Verify(platform.envelope(t, func(fields map[string]any) {
		fields["action"] = ActionRecheckHealth
		fields["arguments"] = map[string]any{"target": "web-01:api"}
	}), testIdentity)
	if refusal != nil || task.Action != ActionRecheckHealth || task.Target != "web-01:api" {
		t.Fatalf("recheck_health: %+v %+v", task, refusal)
	}
}

func TestVerifyAcceptsTheEdgesOfTheTimeWindow(t *testing.T) {
	platform := newSigner(t)
	verifier := newVerifier(t, platform, testPolicy)

	cases := map[string][2]string{
		"expiring this second":            {"2026-11-12T11:55:00Z", "2026-11-12T12:01:00Z"},
		"issued two minutes ahead":        {"2026-11-12T12:03:00Z", "2026-11-12T12:08:00Z"},
		"valid for exactly ten minutes":   {"2026-11-12T12:00:00Z", "2026-11-12T12:10:00Z"},
		"times written with an offset":    {"2026-11-12T13:00:00+01:00", "2026-11-12T13:05:00+01:00"},
		"times written with microseconds": {"2026-11-12T12:00:00.000000Z", "2026-11-12T12:05:00.000000Z"},
	}
	for name, window := range cases {
		t.Run(name, func(t *testing.T) {
			_, refusal := verifier.Verify(platform.envelope(t, func(fields map[string]any) {
				fields["issued_at"], fields["expires_at"] = window[0], window[1]
			}), testIdentity)
			if refusal != nil {
				t.Errorf("refused with %s", refusal.Code)
			}
		})
	}
}

func TestVerifyRefusals(t *testing.T) {
	platform := newSigner(t)
	stranger := newSigner(t)
	set := func(field string, value any) func(map[string]any) {
		return func(fields map[string]any) { fields[field] = value }
	}
	window := func(issued, expires string) func(map[string]any) {
		return func(fields map[string]any) { fields["issued_at"], fields["expires_at"] = issued, expires }
	}
	recheck := func(target string) func(map[string]any) {
		return func(fields map[string]any) {
			fields["action"] = ActionRecheckHealth
			fields["arguments"] = map[string]any{"target": target}
		}
	}
	valid := platform.envelope(t, nil)

	cases := []struct {
		name     string
		envelope api.TaskEnvelope
		policy   Policy
		identity Identity
		want     string
		silent   bool // the refusal carries no task ID, so it is logged and dropped
	}{
		{name: "payload changed after signing", want: RefusalBadSignature, envelope: api.TaskEnvelope{
			KeyID:     testKeyID,
			Payload:   base64.StdEncoding.EncodeToString(payloadJSON(t, set("action", ActionRecheckHealth))),
			Signature: valid.Signature,
		}},
		{name: "signed by another key", want: RefusalBadSignature, envelope: stranger.envelope(t, nil)},
		{name: "signature that is not base64", want: RefusalBadSignature, envelope: api.TaskEnvelope{KeyID: testKeyID, Payload: valid.Payload, Signature: "***"}},
		{name: "empty signature", want: RefusalBadSignature, envelope: api.TaskEnvelope{KeyID: testKeyID, Payload: valid.Payload}},
		{name: "payload that is not base64", want: RefusalBadSignature, silent: true, envelope: api.TaskEnvelope{KeyID: testKeyID, Payload: "***", Signature: valid.Signature}},
		{name: "unsigned payload without a task ID", want: RefusalBadSignature, silent: true, envelope: api.TaskEnvelope{
			KeyID: testKeyID, Payload: base64.StdEncoding.EncodeToString([]byte(`{"action":"clear_cache"}`)), Signature: valid.Signature,
		}},
		{name: "unsigned payload with a task ID that is no ULID", want: RefusalBadSignature, silent: true, envelope: api.TaskEnvelope{
			KeyID: testKeyID, Payload: base64.StdEncoding.EncodeToString(payloadJSON(t, set("task_id", "../heartbeat"))), Signature: valid.Signature,
		}},

		{name: "key ID that is not configured", want: RefusalUnknownKey, envelope: api.TaskEnvelope{KeyID: "k2", Payload: valid.Payload, Signature: valid.Signature}},

		{name: "protocol 2", want: RefusalUnsupportedProtocol, envelope: platform.envelope(t, set("protocol", 2))},
		{name: "protocol missing", want: RefusalUnsupportedProtocol, envelope: platform.envelope(t, func(fields map[string]any) { delete(fields, "protocol") })},
		{name: "field the agent does not know", want: RefusalUnsupportedProtocol, envelope: platform.envelope(t, set("command", "systemctl restart worker"))},
		{name: "content after the payload", want: RefusalUnsupportedProtocol, silent: true, envelope: platform.sign(append(payloadJSON(t, nil), []byte(` {"action":"x"}`)...))},
		{name: "time that is not RFC 3339", want: RefusalUnsupportedProtocol, envelope: platform.envelope(t, set("expires_at", "tomorrow"))},
		{name: "signed payload that is not JSON", want: RefusalUnsupportedProtocol, silent: true, envelope: platform.sign([]byte("clear the cache"))},

		{name: "expired a second ago", want: RefusalExpired, envelope: platform.envelope(t, window("2026-11-12T11:56:00Z", "2026-11-12T12:00:59Z"))},
		{name: "valid for longer than ten minutes", want: RefusalExpired, envelope: platform.envelope(t, window("2026-11-12T12:00:00Z", "2026-11-12T12:10:01Z"))},
		{name: "no expiry", want: RefusalExpired, envelope: platform.envelope(t, func(fields map[string]any) { delete(fields, "expires_at") })},

		{name: "issued more than two minutes ahead", want: RefusalNotYetValid, envelope: platform.envelope(t, window("2026-11-12T12:03:01Z", "2026-11-12T12:08:00Z"))},
		{name: "issued far ahead with a long life", want: RefusalNotYetValid, envelope: platform.envelope(t, window("2026-11-12T13:00:00Z", "2026-11-12T14:00:00Z"))},

		{name: "another agent", want: RefusalWrongAgent, envelope: platform.envelope(t, set("agent_id", "01JCAGENT00000000000000002"))},
		{name: "another agent in another environment", want: RefusalWrongAgent, envelope: valid, identity: Identity{AgentID: "01JCAGENT00000000000000002", EnvironmentID: "01JCENV0000000000000000002"}},
		{name: "another environment", want: RefusalWrongEnvironment, envelope: platform.envelope(t, set("environment_id", "01JCENV0000000000000000002"))},

		{name: "restart requested", want: RefusalUnknownAction, envelope: platform.envelope(t, func(fields map[string]any) {
			fields["action"] = "restart_service"
			fields["arguments"] = map[string]any{"target": "worker"}
		})},
		{name: "action missing", want: RefusalUnknownAction, envelope: platform.envelope(t, func(fields map[string]any) { delete(fields, "action") })},

		{name: "extra argument", want: RefusalInvalidArguments, envelope: platform.envelope(t, set("arguments", map[string]any{"target": "views", "force": true}))},
		{name: "no target", want: RefusalInvalidArguments, envelope: platform.envelope(t, set("arguments", map[string]any{}))},
		{name: "no arguments", want: RefusalInvalidArguments, envelope: platform.envelope(t, func(fields map[string]any) { delete(fields, "arguments") })},
		{name: "null arguments", want: RefusalInvalidArguments, envelope: platform.envelope(t, set("arguments", nil))},
		{name: "arguments as a list", want: RefusalInvalidArguments, envelope: platform.envelope(t, set("arguments", []string{"views"}))},
		{name: "target that is not a string", want: RefusalInvalidArguments, envelope: platform.envelope(t, set("arguments", map[string]any{"target": 7}))},
		{name: "target with a path", want: RefusalInvalidArguments, envelope: platform.envelope(t, set("arguments", map[string]any{"target": "views/../sessions"}))},
		{name: "empty target", want: RefusalInvalidArguments, envelope: platform.envelope(t, set("arguments", map[string]any{"target": ""}))},

		{name: "hook that is not declared", want: RefusalNotEnabled, envelope: platform.envelope(t, set("arguments", map[string]any{"target": "sessions"}))},
		{name: "check of another host", want: RefusalNotEnabled, envelope: platform.envelope(t, recheck("web-02:api"))},
		{name: "check without the host prefix", want: RefusalNotEnabled, envelope: platform.envelope(t, recheck("api"))},
		{name: "check that is not listed", want: RefusalNotEnabled, envelope: platform.envelope(t, recheck("web-01:postgres"))},
		{name: "action with no local target", want: RefusalNotEnabled, envelope: valid, policy: Policy{Prefix: "web-01", Rechecks: []string{"api"}}},
		{name: "action with no local target and bad arguments", want: RefusalNotEnabled, policy: Policy{Prefix: "web-01", Rechecks: []string{"api"}},
			envelope: platform.envelope(t, set("arguments", map[string]any{"target": "views", "force": true}))},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			policy, identity := testCase.policy, testCase.identity
			if policy.Prefix == "" {
				policy = testPolicy
			}
			if identity == (Identity{}) {
				identity = testIdentity
			}

			task, refusal := newVerifier(t, platform, policy).Verify(testCase.envelope, identity)
			if task != nil || refusal == nil {
				t.Fatalf("the envelope was accepted: %+v", task)
			}
			if refusal.Code != testCase.want {
				t.Errorf("refused with %s, want %s", refusal.Code, testCase.want)
			}
			if refusal.Reportable() == testCase.silent {
				t.Errorf("reportable = %v, want %v (%+v)", refusal.Reportable(), !testCase.silent, refusal)
			}
			if refusal.Reportable() && (refusal.TaskID != testTaskID || refusal.Nonce != testNonce) {
				t.Errorf("the refusal names another task: %+v", refusal)
			}
		})
	}
}

func TestVerifyRefusesAReplayAfterTheStoreIsReopened(t *testing.T) {
	platform := newSigner(t)
	stateDir := t.TempDir()
	first := NewVerifier(platform.keys(), testPolicy, openStore(t, stateDir))
	first.now = func() time.Time { return testNow }

	task, refusal := first.Verify(platform.envelope(t, nil), testIdentity)
	if refusal != nil {
		t.Fatalf("first delivery refused: %+v", refusal)
	}
	if again, refusal := first.Verify(platform.envelope(t, nil), testIdentity); refusal != nil || again == nil {
		t.Fatalf("a task that was never received is not a replay: %+v", refusal)
	}
	if err := first.replay.Remember(task, testNow); err != nil {
		t.Fatal(err)
	}

	restarted := NewVerifier(platform.keys(), testPolicy, openStore(t, stateDir))
	restarted.now = func() time.Time { return testNow.Add(time.Minute) }
	replays := map[string]func(fields map[string]any){
		"the same envelope":                 nil,
		"the same task ID with a new nonce": func(fields map[string]any) { fields["nonce"] = "ffffffffffffffffffffffffffffffff" },
		"the same nonce with a new task ID": func(fields map[string]any) { fields["task_id"] = "01JCTASK000000000000000002" },
		"the same task with another action": func(fields map[string]any) { fields["action"] = ActionRecheckHealth },
	}
	for name, change := range replays {
		t.Run(name, func(t *testing.T) {
			_, refusal := restarted.Verify(platform.envelope(t, change), testIdentity)
			if refusal == nil || refusal.Code != RefusalReplayed {
				t.Errorf("want replayed, got %+v", refusal)
			}
		})
	}

	_, refusal = restarted.Verify(platform.envelope(t, func(fields map[string]any) {
		fields["task_id"] = "01JCTASK000000000000000002"
		fields["nonce"] = "ffffffffffffffffffffffffffffffff"
	}), testIdentity)
	if refusal != nil {
		t.Errorf("a new task was refused after the restart: %+v", refusal)
	}
}

type goldenFixture struct {
	PublicKey   string           `json:"public_key"`
	KeyID       string           `json:"key_id"`
	PayloadJSON string           `json:"payload_json"`
	Envelope    api.TaskEnvelope `json:"envelope"`
}

// The fixture was signed by the platform's PHP sodium signer; its secret key was discarded.
func TestGoldenEnvelopeFromThePlatform(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "testdata", "envelope.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden goldenFixture
	if err := decodeStrict(content, &golden); err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	public, err := base64.StdEncoding.DecodeString(golden.PublicKey)
	if err != nil || len(public) != ed25519.PublicKeySize {
		t.Fatalf("fixture public key: %v, %d bytes", err, len(public))
	}
	if signed, _ := base64.StdEncoding.DecodeString(golden.Envelope.Payload); string(signed) != golden.PayloadJSON {
		t.Fatalf("the envelope does not carry the fixture's payload bytes:\n%s", signed)
	}

	identity := Identity{AgentID: "01JCAGENT00000000000000000", EnvironmentID: "01JCENVIRONMENT00000000000"}
	policy := Policy{Prefix: "web-01", Hooks: []Hook{{Name: "views", URL: "http://127.0.0.1:8080/cache/clear", Timeout: 30 * time.Second}}}
	verifier := NewVerifier(map[string]ed25519.PublicKey{golden.KeyID: public}, policy, openStore(t, t.TempDir()))
	verifier.now = func() time.Time { return time.Date(2026, 11, 12, 12, 1, 0, 0, time.UTC) }

	task, refusal := verifier.Verify(golden.Envelope, identity)
	if refusal != nil {
		t.Fatalf("the platform's envelope was refused: %s", refusal.Code)
	}
	want := Task{
		ID:                "01JCRECOVERYTASK000000000A",
		Nonce:             "9f86d081884c7d659a2feaa0c55ad015",
		Action:            ActionClearCache,
		Target:            "views",
		VerificationCheck: "web-01:listener",
		Timeout:           time.Minute,
		ExpiresAt:         time.Date(2026, 11, 12, 12, 5, 0, 0, time.UTC),
	}
	if *task != want {
		t.Errorf("task:\n got %+v\nwant %+v", *task, want)
	}

	tampered := golden.Envelope
	signed := []byte(golden.PayloadJSON)
	signed[len(signed)-2] ^= 1
	tampered.Payload = base64.StdEncoding.EncodeToString(signed)
	if _, refusal := verifier.Verify(tampered, identity); refusal == nil || refusal.Code != RefusalBadSignature {
		t.Errorf("one changed byte must be a bad signature, got %+v", refusal)
	}

	verifier.now = func() time.Time { return time.Date(2026, 11, 12, 12, 5, 1, 0, time.UTC) }
	if _, refusal := verifier.Verify(golden.Envelope, identity); refusal == nil || refusal.Code != RefusalExpired {
		t.Errorf("after its expiry the envelope must be refused as expired, got %+v", refusal)
	}
}
