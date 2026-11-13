package runner_test

// Tests for the recovery loop against the fake platform, which keeps to the recovery task
// protocol: it serves a task until it is received, checks every report's shape and nonce, and
// refuses transitions out of order.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alimrun/autoheal-agent/internal/api"
	"github.com/alimrun/autoheal-agent/internal/check"
	"github.com/alimrun/autoheal-agent/internal/credential"
	"github.com/alimrun/autoheal-agent/internal/recovery"
	"github.com/alimrun/autoheal-agent/internal/runner"
)

const (
	platformKeyID = "k1"
	hookName      = "views"
)

// ---------------------------------------------------------------------------
// Fake platform: recovery

type fakeTask struct {
	id       string
	nonce    string
	status   string
	envelope api.TaskEnvelope
}

type taskReport struct {
	taskID string
	at     time.Time
	api.TaskReport
}

type fakeRecovery struct {
	supported                 bool
	paused                    bool
	tasksWhilePaused          bool // a platform that breaks the protocol, to prove the agent holds back
	serveAll                  bool // offer every waiting task at once instead of one
	forgetsTasks              bool // answer every report with 404
	conflictOn                string
	dropStage                 string // reports of this stage are lost before the platform sees them
	loseAnswerTo              string // the first report of this stage is recorded, then its answer is lost
	unscoped                  map[string]bool
	rotationKeepsScopeMissing bool
	tasks                     []*fakeTask
	reports                   []taskReport
}

var transitions = map[string][]string{
	"sent":     {api.StageReceived, api.StageRefused},
	"received": {api.StageRunning, api.StageRefused},
	"running":  {api.StageFinished, api.StageFailed},
}

// modeFor lets a test lose one stage's reports, or the answer to the first of them.
func (state *fakeRecovery) modeFor(current mode, body []byte) mode {
	var report api.TaskReport
	_ = json.Unmarshal(body, &report)
	switch report.Stage {
	case "":
		return current
	case state.dropStage:
		return dropConnection
	case state.loseAnswerTo:
		state.loseAnswerTo = ""
		return storeThenDrop
	}
	return current
}

func scopeMissing() reply {
	return reply{status: http.StatusForbidden, code: "agent_scope_missing_403", message: "This credential lacks the recovery scope."}
}

func notFound() reply {
	return reply{status: http.StatusNotFound, code: "http_404", message: "Not Found."}
}

// answerTasks is called with mu held.
func (fake *fakePlatform) answerTasks(token string) reply {
	state := &fake.recovery
	switch {
	case !state.supported:
		return notFound()
	case state.unscoped[token]:
		return scopeMissing()
	}

	offered := []api.TaskEnvelope{}
	if !state.paused || state.tasksWhilePaused {
		for _, task := range state.tasks {
			if task.status == "sent" && (len(offered) == 0 || state.serveAll) {
				offered = append(offered, task.envelope)
			}
		}
	}
	return reply{status: http.StatusOK, code: "agent_tasks_200", message: "Tasks.", content: map[string]any{"paused": state.paused, "tasks": offered}}
}

// answerTaskResult is called with mu held.
func (fake *fakePlatform) answerTaskResult(token, path string, body []byte) reply {
	state := &fake.recovery
	switch {
	case !state.supported:
		return notFound()
	case state.unscoped[token]:
		return scopeMissing()
	}

	taskID := strings.TrimSuffix(strings.TrimPrefix(path, pathTasks+"/"), "/result")
	var report api.TaskReport
	_ = json.Unmarshal(body, &report)
	state.reports = append(state.reports, taskReport{taskID: taskID, at: time.Now(), TaskReport: report})

	index := slices.IndexFunc(state.tasks, func(task *fakeTask) bool { return task.id == taskID })
	if index < 0 || state.forgetsTasks {
		return notFound()
	}
	task := state.tasks[index]
	if problem := reportProblem(body, task.nonce); problem != "" {
		return reply{status: http.StatusUnprocessableEntity, code: "validation_failed_422", message: problem, errors: map[string][]string{"report": {problem}}}
	}
	if report.Stage == state.conflictOn || !slices.Contains(transitions[task.status], report.Stage) {
		return reply{status: http.StatusConflict, code: "recovery_task_conflict_409", message: "Invalid transition.", content: map[string]any{"status": task.status}}
	}
	task.status = report.Stage
	return reply{status: http.StatusOK, code: "agent_task_result_200", message: "Recorded.", content: map[string]any{"status": task.status}}
}

// reportProblem holds a report to the protocol: known keys only, the right nonce, a reason
// exactly when the stage needs one, and bounded evidence.
func reportProblem(body []byte, nonce string) string {
	var report map[string]json.RawMessage
	if err := json.Unmarshal(body, &report); err != nil {
		return "the report is not a JSON object"
	}
	for key := range report {
		if !slices.Contains([]string{"nonce", "stage", "reason_code", "evidence"}, key) {
			return "unknown key " + key
		}
	}
	var sent api.TaskReport
	_ = json.Unmarshal(body, &sent)
	if sent.Nonce != nonce {
		return "wrong nonce"
	}
	if needsReason := sent.Stage == api.StageFailed || sent.Stage == api.StageRefused; needsReason != (sent.ReasonCode != "") {
		return "reason_code does not fit the stage"
	}

	var evidence map[string]json.RawMessage
	_ = json.Unmarshal(report["evidence"], &evidence)
	for key := range evidence {
		if !slices.Contains([]string{"duration_ms", "outcome", "http_status", "check_status"}, key) {
			return "unknown evidence key " + key
		}
	}
	if sent.Evidence != nil && len(sent.Evidence.Outcome) > 64 {
		return "outcome is longer than 64 characters"
	}
	return ""
}

func (fake *fakePlatform) reportsFor(taskID string) []taskReport {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var matching []taskReport
	for _, report := range fake.recovery.reports {
		if report.taskID == taskID {
			matching = append(matching, report)
		}
	}
	return matching
}

func (fake *fakePlatform) allReports() []taskReport {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return slices.Clone(fake.recovery.reports)
}

func (fake *fakePlatform) taskStatus(taskID string) string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, task := range fake.recovery.tasks {
		if task.id == taskID {
			return task.status
		}
	}
	return ""
}

func (fake *fakePlatform) eventsFor(name string) []sentEvent {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var matching []sentEvent
	for _, stored := range fake.events {
		if stored.Body.Name == name {
			matching = append(matching, stored)
		}
	}
	return matching
}

func (fake *fakePlatform) lastHeartbeat() map[string]any {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.heartbeats) == 0 {
		return nil
	}
	return fake.heartbeats[len(fake.heartbeats)-1]
}

func stages(reports []taskReport) string {
	names := make([]string, 0, len(reports))
	for _, report := range reports {
		name := report.Stage
		if report.ReasonCode != "" {
			name += ":" + report.ReasonCode
		}
		names = append(names, name)
	}
	return strings.Join(names, " ")
}

func okPolls(fake *fakePlatform) int {
	return countWhere(fake.requestsTo(pathTasks), func(seen request) bool { return seen.status == http.StatusOK })
}

// ---------------------------------------------------------------------------
// Agent harness: recovery

type hook struct {
	server *httptest.Server

	mu     sync.Mutex
	status int
	bodies []string
	tokens []string
	hold   chan struct{} // when set, the hook does not answer until the request is abandoned
}

func (target *hook) ServeHTTP(writer http.ResponseWriter, incoming *http.Request) {
	body, _ := io.ReadAll(incoming.Body)
	target.mu.Lock()
	target.bodies = append(target.bodies, string(body))
	target.tokens = append(target.tokens, incoming.Header.Get("Authorization"))
	status, hold := target.status, target.hold
	target.mu.Unlock()

	if hold != nil {
		select {
		case <-incoming.Context().Done():
		case <-hold:
		}
		return
	}
	writer.WriteHeader(status)
}

func (target *hook) calls() []string {
	target.mu.Lock()
	defer target.mu.Unlock()
	return slices.Clone(target.bodies)
}

type recoverySetup struct {
	public  ed25519.PublicKey
	private ed25519.PrivateKey
	policy  recovery.Policy
	hook    *hook
	issued  int
}

// enableRecovery turns recovery on for the agents this harness starts: a platform key made for
// the test, the check "api" open to recheck_health and one cache hook named "views".
func (h *harness) enableRecovery() *recoverySetup {
	h.t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		h.t.Fatal(err)
	}
	target := &hook{status: http.StatusNoContent}
	target.server = httptest.NewServer(target)
	h.t.Cleanup(target.server.Close)

	h.recovery = &recoverySetup{
		public:  public,
		private: private,
		hook:    target,
		policy: recovery.Policy{
			Prefix:   "web-01",
			Rechecks: []string{"api"},
			Hooks:    []recovery.Hook{{Name: hookName, URL: target.server.URL + "/internal/cache/clear", Timeout: 5 * time.Second}},
		},
	}
	h.fake.with(func(fake *fakePlatform) { fake.recovery.supported = true })
	return h.recovery
}

// build opens the replay store again, as a restart does.
func (setup *recoverySetup) build(h *harness) *runner.Recovery {
	if setup == nil {
		return nil
	}
	replay, err := recovery.OpenReplayStore(h.stateDir)
	if err != nil {
		h.t.Fatalf("opening the replay store: %v", err)
	}
	keys := map[string]ed25519.PublicKey{platformKeyID: setup.public}
	return &runner.Recovery{
		Verifier: recovery.NewVerifier(keys, setup.policy, replay),
		Replay:   replay,
		Registry: recovery.NewRegistry(setup.policy, nil, "autoheal-agent/test recovery"),
	}
}

// sign builds a task for the harness's agent that clears the cache "views", after change.
func (setup *recoverySetup) sign(t *testing.T, change func(fields map[string]any)) *fakeTask {
	t.Helper()
	setup.issued++
	now := time.Now().UTC()
	fields := map[string]any{
		"protocol":           1,
		"task_id":            fmt.Sprintf("01JCTASK%018d", setup.issued),
		"nonce":              fmt.Sprintf("%032x", setup.issued),
		"agent_id":           "agent-1",
		"environment_id":     "environment-1",
		"action":             recovery.ActionClearCache,
		"arguments":          map[string]any{"target": hookName},
		"policy_version":     3,
		"catalog_version":    1,
		"verification_check": checkName,
		"timeout_seconds":    60,
		"issued_at":          now.Add(-time.Second).Format(time.RFC3339),
		"expires_at":         now.Add(5 * time.Minute).Format(time.RFC3339),
	}
	if change != nil {
		change(fields)
	}
	signed, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeTask{
		id:     fields["task_id"].(string),
		nonce:  fields["nonce"].(string),
		status: "sent",
		envelope: api.TaskEnvelope{
			KeyID:     platformKeyID,
			Payload:   base64.StdEncoding.EncodeToString(signed),
			Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(setup.private, signed)),
		},
	}
}

func (h *harness) offer(tasks ...*fakeTask) {
	h.fake.with(func(fake *fakePlatform) { fake.recovery.tasks = append(fake.recovery.tasks, tasks...) })
}

// steadyProbe always reports the same status and counts its runs.
type steadyProbe struct {
	mu     sync.Mutex
	status check.Status
	runs   int
}

func (probe *steadyProbe) Run(context.Context) check.Result {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	probe.runs++
	return check.Result{Status: probe.status, Detail: "steady"}
}

func (probe *steadyProbe) count() int {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	return probe.runs
}

// quietCheck is the check "web-01:api" on a schedule that reports nothing during a test: one
// scheduled run, which the reporter holds back for a second, confirming one.
func quietCheck(status check.Status) (*check.Check, *steadyProbe) {
	probe := &steadyProbe{status: status}
	return &check.Check{
		Name:         checkName,
		Kind:         check.KindHTTP,
		Target:       "http://127.0.0.1:8080/health",
		Interval:     time.Hour,
		Timeout:      time.Second,
		DownSeverity: "critical",
		Probe:        probe,
	}, probe
}

// recoveryOptions keeps the schedule and the flush timer out of the way, so the only events
// that can reach the platform are the ones saved on demand.
func recoveryOptions() runner.Options {
	options := testOptions()
	options.Confirm = 2
	options.FlushInterval = time.Hour
	return options
}

func set(field string, value any) func(map[string]any) {
	return func(fields map[string]any) { fields[field] = value }
}

func settled(h *harness, polls int) func() bool {
	from := okPolls(h.fake)
	return func() bool { return okPolls(h.fake) >= from+polls }
}

// ---------------------------------------------------------------------------
// Tests

func TestRecoveryOffNeverAsksForTasks(t *testing.T) {
	h := newHarness(t)
	h.fake.with(func(fake *fakePlatform) { fake.recovery.supported = true })
	chk, _ := quietCheck(check.Up)
	run := h.start(testOptions(), chk)

	eventually(t, 2*time.Second, "five heartbeats", func() bool {
		return countWhere(h.fake.requestsTo(pathHeartbeat), func(seen request) bool { return seen.status == http.StatusOK }) >= 5
	})
	run.stop(t)

	if asked := len(h.fake.requestsTo(pathTasks)) + len(h.fake.requestsTo(pathTaskResult)); asked != 0 {
		t.Errorf("an agent with recovery off called the task endpoints %d times", asked)
	}
	if _, present := h.fake.lastHeartbeat()["recovery"]; present {
		t.Errorf("the heartbeat reports a recovery capability while recovery is off: %v", h.fake.lastHeartbeat())
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, recovery.ReplayFileName)); !os.IsNotExist(err) {
		t.Errorf("an agent with recovery off should keep no replay store: %v", err)
	}
}

func TestRecoveryClearsACacheAndVerifiesAtOnce(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	chk, probe := quietCheck(check.Up)
	task := setup.sign(t, nil)
	h.offer(task)
	run := h.start(recoveryOptions(), chk)

	eventually(t, 3*time.Second, "the verification event at the platform", func() bool { return len(h.fake.eventsFor(checkName)) == 1 })
	eventually(t, time.Second, "polling to go on", settled(h, 3))
	run.stop(t)

	reports := h.fake.reportsFor(task.id)
	if stages(reports) != "received running finished" || h.fake.taskStatus(task.id) != "finished" {
		t.Fatalf("reports %q, status %q", stages(reports), h.fake.taskStatus(task.id))
	}
	if len(h.fake.allReports()) != 3 {
		t.Errorf("reports for other tasks: %q", stages(h.fake.allReports()))
	}
	for _, report := range reports[:2] {
		if report.Evidence != nil {
			t.Errorf("%s carries evidence: %+v", report.Stage, report.Evidence)
		}
	}
	evidence := reports[2].Evidence
	if evidence == nil || evidence.HTTPStatus != http.StatusNoContent || evidence.Outcome != "hook answered 204" || evidence.DurationMs == nil || evidence.CheckStatus != "" {
		t.Errorf("finished evidence: %+v", evidence)
	}

	if calls := setup.hook.calls(); len(calls) != 1 || calls[0] != `{"task_id":"`+task.id+`"}` {
		t.Errorf("the hook should be called once with only the task ID: %v", calls)
	}

	verification := h.fake.eventsFor(checkName)[0]
	if verification.Body.Status != "up" {
		t.Errorf("verification event: %+v", verification)
	}
	if finishedAt := reports[2].at.Truncate(time.Millisecond); verification.OccurredAt.Before(finishedAt) {
		t.Errorf("the verification check ran at %s, before the finished report at %s", verification.OccurredAt, finishedAt)
	}
	if runs := probe.count(); runs != 2 {
		t.Errorf("the check ran %d times, want the scheduled run and one on demand", runs)
	}

	heartbeat, _ := json.Marshal(h.fake.lastHeartbeat())
	capability, _ := json.Marshal(h.fake.lastHeartbeat()["recovery"])
	if string(capability) != `{"actions":["recheck_health","clear_cache"],"protocol":1}` {
		t.Errorf("capability: %s", capability)
	}
	if strings.Contains(string(heartbeat), hookName) || strings.Contains(string(heartbeat), "cache/clear") {
		t.Errorf("the heartbeat names a target: %s", heartbeat)
	}

	info, err := os.Stat(filepath.Join(h.stateDir, recovery.ReplayFileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("replay store: %v %v", info, err)
	}
	if polls := h.fake.requestsTo(pathTasks); polls[0].method != http.MethodGet {
		t.Errorf("tasks are fetched with %s", polls[0].method)
	}
}

func TestRecoveryChecksHealthAgain(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	chk, probe := quietCheck(check.Degraded)
	task := setup.sign(t, func(fields map[string]any) {
		fields["action"] = recovery.ActionRecheckHealth
		fields["arguments"] = map[string]any{"target": checkName}
	})
	h.offer(task)
	run := h.start(recoveryOptions(), chk)

	eventually(t, 3*time.Second, "the recheck and the verification events", func() bool { return len(h.fake.eventsFor(checkName)) == 2 })
	run.stop(t)

	reports := h.fake.reportsFor(task.id)
	if stages(reports) != "received running finished" {
		t.Fatalf("reports %q", stages(reports))
	}
	evidence := reports[2].Evidence
	if evidence == nil || evidence.CheckStatus != "degraded" || evidence.Outcome != "check is degraded" || evidence.HTTPStatus != 0 {
		t.Errorf("finished evidence: %+v", evidence)
	}
	if len(setup.hook.calls()) != 0 {
		t.Error("a recheck must not call the cache hook")
	}
	if runs := probe.count(); runs != 3 {
		t.Errorf("the check ran %d times, want the scheduled run, the recheck and the verification", runs)
	}
	for _, stored := range h.fake.eventsFor(checkName) {
		if stored.Body.Status != "degraded" {
			t.Errorf("event: %+v", stored)
		}
	}
}

func TestRecoveryRefusedTaskIsReportedWithItsCode(t *testing.T) {
	cases := []struct {
		name   string
		change func(fields map[string]any)
		tamper func(task *fakeTask)
		want   string
	}{
		{name: "restart requested", want: recovery.RefusalUnknownAction, change: func(fields map[string]any) {
			fields["action"] = "restart_service"
			fields["arguments"] = map[string]any{"target": "worker"}
		}},
		{name: "another agent", want: recovery.RefusalWrongAgent, change: set("agent_id", "agent-2")},
		{name: "another environment", want: recovery.RefusalWrongEnvironment, change: set("environment_id", "environment-2")},
		{name: "cache that is not declared", want: recovery.RefusalNotEnabled, change: set("arguments", map[string]any{"target": "sessions"})},
		{name: "extra argument", want: recovery.RefusalInvalidArguments, change: set("arguments", map[string]any{"target": hookName, "force": true})},
		{name: "expired", want: recovery.RefusalExpired, change: set("expires_at", time.Now().UTC().Add(-time.Second).Format(time.RFC3339))},
		{name: "issued in the future", want: recovery.RefusalNotYetValid, change: func(fields map[string]any) {
			fields["issued_at"] = time.Now().UTC().Add(3 * time.Minute).Format(time.RFC3339)
			fields["expires_at"] = time.Now().UTC().Add(8 * time.Minute).Format(time.RFC3339)
		}},
		{name: "protocol 2", want: recovery.RefusalUnsupportedProtocol, change: set("protocol", 2)},
		{name: "unknown key", want: recovery.RefusalUnknownKey, tamper: func(task *fakeTask) { task.envelope.KeyID = "k2" }},
		{name: "tampered payload", want: recovery.RefusalBadSignature, tamper: func(task *fakeTask) {
			signed, _ := base64.StdEncoding.DecodeString(task.envelope.Payload)
			signed = []byte(strings.Replace(string(signed), `"target":"views"`, `"target":"viewz"`, 1))
			task.envelope.Payload = base64.StdEncoding.EncodeToString(signed)
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			setup := h.enableRecovery()
			chk, probe := quietCheck(check.Up)
			task := setup.sign(t, testCase.change)
			if testCase.tamper != nil {
				testCase.tamper(task)
			}
			h.offer(task)
			run := h.start(recoveryOptions(), chk)

			eventually(t, 3*time.Second, "the refusal", func() bool { return len(h.fake.reportsFor(task.id)) >= 1 })
			eventually(t, time.Second, "further polls", settled(h, 5))
			run.stop(t)

			if got := stages(h.fake.allReports()); got != "refused:"+testCase.want {
				t.Errorf("reports %q, want one refusal with %s", got, testCase.want)
			}
			if h.fake.taskStatus(task.id) != "refused" {
				t.Errorf("the platform holds the task as %q", h.fake.taskStatus(task.id))
			}
			if len(setup.hook.calls()) != 0 || probe.count() != 1 || len(h.fake.eventsFor(checkName)) != 0 {
				t.Errorf("a refused task was acted on: %d hook calls, %d check runs", len(setup.hook.calls()), probe.count())
			}
			if h.logs.Count("refused a recovery task") != 1 {
				t.Errorf("the refusal should be logged once:\n%s", h.logs)
			}
		})
	}
}

func TestRecoveryReportsARefusalOnceWhileTheTaskKeepsComing(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	h.fake.with(func(fake *fakePlatform) { fake.recovery.forgetsTasks = true })
	chk, _ := quietCheck(check.Up)
	task := setup.sign(t, set("agent_id", "agent-2"))
	h.offer(task)
	run := h.start(recoveryOptions(), chk)

	eventually(t, 3*time.Second, "the refusal", func() bool { return len(h.fake.reportsFor(task.id)) >= 1 })
	eventually(t, time.Second, "the task offered ten more times", settled(h, 10))
	run.stop(t)

	if got := stages(h.fake.allReports()); got != "refused:wrong_agent" {
		t.Errorf("reports %q, want the refusal reported once", got)
	}
}

func TestRecoveryDropsAnEnvelopeItCannotName(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	chk, _ := quietCheck(check.Up)
	task := setup.sign(t, nil)
	task.envelope.Payload = base64.StdEncoding.EncodeToString([]byte("not a payload"))
	h.offer(task)
	run := h.start(recoveryOptions(), chk)

	eventually(t, 3*time.Second, "the envelope to be dropped", func() bool { return h.logs.Count("dropped a recovery task that cannot be reported") >= 1 })
	run.stop(t)

	if len(h.fake.allReports()) != 0 || len(setup.hook.calls()) != 0 {
		t.Errorf("reports %q, hook calls %d", stages(h.fake.allReports()), len(setup.hook.calls()))
	}
}

func TestRecoveryRefusesAReplayAfterARestart(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	chk, _ := quietCheck(check.Up)
	task := setup.sign(t, nil)
	h.offer(task)
	first := h.start(recoveryOptions(), chk)
	eventually(t, 3*time.Second, "the task to finish", func() bool { return h.fake.taskStatus(task.id) == "finished" })
	first.stop(t)

	h.fake.with(func(fake *fakePlatform) { fake.recovery.tasks[0].status = "sent" })
	second := h.start(recoveryOptions(), chk)
	eventually(t, 3*time.Second, "the replay to be refused", func() bool { return len(h.fake.reportsFor(task.id)) >= 4 })
	eventually(t, time.Second, "further polls", settled(h, 3))
	second.stop(t)

	if got := stages(h.fake.reportsFor(task.id)); got != "received running finished refused:replayed" {
		t.Errorf("reports %q", got)
	}
	if calls := len(setup.hook.calls()); calls != 1 {
		t.Errorf("the hook was called %d times; a replayed task must do nothing", calls)
	}
}

func TestRecoveryHoldsOneTaskAtATime(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	h.fake.with(func(fake *fakePlatform) { fake.recovery.serveAll = true })
	chk, _ := quietCheck(check.Up)
	held, extra := setup.sign(t, nil), setup.sign(t, nil)
	h.offer(held, extra)
	run := h.start(recoveryOptions(), chk)

	eventually(t, 3*time.Second, "the first task to finish", func() bool { return h.fake.taskStatus(held.id) == "finished" })
	eventually(t, time.Second, "further polls", settled(h, 3))
	run.stop(t)

	if got := stages(h.fake.reportsFor(held.id)); got != "received running finished" {
		t.Errorf("held task: %q", got)
	}
	if got := stages(h.fake.reportsFor(extra.id)); got != "refused:busy" {
		t.Errorf("second task: %q, want it refused as busy", got)
	}
	if got := stages(h.fake.allReports()); got != "received refused:busy running finished" {
		t.Errorf("order of reports: %q", got)
	}
	if calls := len(setup.hook.calls()); calls != 1 {
		t.Errorf("the hook was called %d times", calls)
	}
}

func TestRecoveryPauseIsHonoured(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	h.fake.with(func(fake *fakePlatform) { fake.recovery.paused, fake.recovery.tasksWhilePaused = true, true })
	chk, _ := quietCheck(check.Up)
	task := setup.sign(t, nil)
	h.offer(task)
	run := h.start(recoveryOptions(), chk)

	started := time.Now()
	eventually(t, 2*time.Second, "ten polls while paused", settled(h, 10))
	if waited := time.Since(started); waited > time.Second {
		t.Errorf("ten polls while paused took %s; a pause must not slow polling", waited)
	}
	if len(h.fake.allReports()) != 0 || len(setup.hook.calls()) != 0 || h.fake.taskStatus(task.id) != "sent" {
		t.Fatalf("a task was acted on during a pause: reports %q, hook calls %d", stages(h.fake.allReports()), len(setup.hook.calls()))
	}

	h.fake.with(func(fake *fakePlatform) { fake.recovery.paused = false })
	eventually(t, 3*time.Second, "the task to finish after the pause", func() bool { return h.fake.taskStatus(task.id) == "finished" })
	run.stop(t)

	if got := stages(h.fake.reportsFor(task.id)); got != "received running finished" {
		t.Errorf("reports %q", got)
	}
	if h.logs.Count("AutoHeal paused recovery") != 1 || h.logs.Count("AutoHeal resumed recovery") != 1 {
		t.Errorf("the pause and its end should each be logged once:\n%s", h.logs)
	}
}

func TestRecoveryPollsSlowlyWhileThePlatformHasNoTaskEndpoint(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	h.fake.with(func(fake *fakePlatform) { fake.recovery.supported = false })
	options := recoveryOptions()
	chk, _ := quietCheck(check.Up)
	run := h.start(options, chk)

	eventually(t, 3*time.Second, "three polls answered 404", func() bool { return len(h.fake.requestsTo(pathTasks)) >= 3 })
	polls := h.fake.requestsTo(pathTasks)
	for index := 1; index < 3; index++ {
		if polls[index].status != http.StatusNotFound {
			t.Fatalf("poll %d was answered %d", index, polls[index].status)
		}
		if gap := polls[index].at.Sub(polls[index-1].at); gap < options.TaskPollUnsupported-20*time.Millisecond {
			t.Errorf("polls %d and %d were %s apart, want the slow pace of %s", index-1, index, gap, options.TaskPollUnsupported)
		}
	}
	if h.logs.Count("no recovery support") != 1 {
		t.Errorf("the slow pace should be logged once:\n%s", h.logs)
	}
	if heartbeats := countWhere(h.fake.requestsTo(pathHeartbeat), func(seen request) bool { return seen.status == http.StatusOK }); heartbeats < 10 {
		t.Errorf("only %d heartbeats arrived while task polling was slow", heartbeats)
	}

	task := setup.sign(t, nil)
	h.offer(task)
	h.fake.with(func(fake *fakePlatform) { fake.recovery.supported = true })
	eventually(t, 3*time.Second, "the task to finish once the platform supports recovery", func() bool { return h.fake.taskStatus(task.id) == "finished" })
	started := time.Now()
	eventually(t, time.Second, "polling at the normal pace again", settled(h, 5))
	if waited := time.Since(started); waited > options.TaskPollUnsupported {
		t.Errorf("five polls took %s after support returned", waited)
	}
	run.stop(t)
}

func TestRecoveryMissingScopeRotatesTheCredentialOnce(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	h.fake.with(func(fake *fakePlatform) { fake.recovery.unscoped[initialToken] = true })
	chk, _ := quietCheck(check.Up)
	task := setup.sign(t, nil)
	h.offer(task)
	run := h.start(recoveryOptions(), chk)

	eventually(t, 3*time.Second, "the task to finish with the rotated credential", func() bool { return h.fake.taskStatus(task.id) == "finished" })
	eventually(t, time.Second, "further polls", settled(h, 5))
	run.stop(t)

	h.fake.mu.Lock()
	rotations := slices.Clone(h.fake.rotations)
	savedFirst := h.fake.savedBeforeUse["token-1"]
	h.fake.mu.Unlock()
	if len(rotations) != 1 || rotations[0].oldToken != initialToken {
		t.Fatalf("rotations: %+v, want exactly one from the first credential", rotations)
	}
	if !savedFirst {
		t.Error("the rotated credential was used before it was saved")
	}

	polls := h.fake.requestsTo(pathTasks)
	if polls[0].status != http.StatusForbidden || polls[0].token != initialToken || polls[1].status != http.StatusOK || polls[1].token != "token-1" {
		t.Errorf("first polls: %+v %+v, want a 403 then a retry with the new credential", polls[0], polls[1])
	}
	if refusedPolls := countWhere(polls, func(seen request) bool { return seen.status == http.StatusForbidden }); refusedPolls != 1 {
		t.Errorf("%d polls were refused, want one", refusedPolls)
	}
	saved, _, err := credential.Load(h.stateDir)
	if err != nil || saved.Token != "token-1" {
		t.Errorf("credential file: %v", err)
	}
	if h.logs.Count("refused this agent's credential") != 0 {
		t.Error("a missing recovery scope must not stop heartbeats and telemetry")
	}
}

func TestRecoveryMissingScopeThatPersistsBacksOff(t *testing.T) {
	h := newHarness(t)
	h.enableRecovery()
	h.fake.with(func(fake *fakePlatform) {
		fake.recovery.unscoped[initialToken] = true
		fake.recovery.rotationKeepsScopeMissing = true
	})
	options := recoveryOptions()
	chk, _ := quietCheck(check.Up)
	run := h.start(options, chk)

	eventually(t, 3*time.Second, "eight refused polls", func() bool { return len(h.fake.requestsTo(pathTasks)) >= 8 })
	run.stop(t)

	h.fake.mu.Lock()
	rotations := len(h.fake.rotations)
	h.fake.mu.Unlock()
	if rotations != 1 {
		t.Errorf("the credential was rotated %d times, want once", rotations)
	}
	polls := h.fake.requestsTo(pathTasks)
	for index, seen := range polls {
		if seen.status != http.StatusForbidden {
			t.Errorf("poll %d was answered %d", index, seen.status)
		}
	}
	if last := polls[len(polls)-1].at.Sub(polls[len(polls)-2].at); last < options.BackoffMax/2-5*time.Millisecond {
		t.Errorf("the last two refused polls were %s apart, want a backoff wait near %s", last, options.BackoffMax)
	}
	if h.logs.Count("still lacks the recovery scope") != 1 {
		t.Errorf("the missing scope should be logged once:\n%s", h.logs)
	}
	if heartbeats := countWhere(h.fake.requestsTo(pathHeartbeat), func(seen request) bool { return seen.status == http.StatusOK }); heartbeats < 3 {
		t.Errorf("only %d heartbeats arrived while the scope was missing", heartbeats)
	}
}

func TestRecoveryLostReceiptAnswerDoesNotRunTheTaskTwice(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	h.fake.setRule(pathTaskResult, storeThenDrop, 1)
	chk, _ := quietCheck(check.Up)
	task := setup.sign(t, nil)
	h.offer(task)
	run := h.start(recoveryOptions(), chk)

	eventually(t, 3*time.Second, "the task to finish", func() bool { return h.fake.taskStatus(task.id) == "finished" })
	eventually(t, time.Second, "further polls", settled(h, 3))
	run.stop(t)

	if got := stages(h.fake.reportsFor(task.id)); got != "received received running finished" {
		t.Errorf("reports %q, want the receipt sent again and then the task carried on", got)
	}
	if calls := len(setup.hook.calls()); calls != 1 {
		t.Errorf("the hook was called %d times", calls)
	}
}

func TestRecoveryDoesNotActWhenThePlatformRefusesTheStart(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	h.fake.with(func(fake *fakePlatform) { fake.recovery.conflictOn = api.StageRunning })
	chk, probe := quietCheck(check.Up)
	task := setup.sign(t, nil)
	h.offer(task)
	run := h.start(recoveryOptions(), chk)

	eventually(t, 3*time.Second, "the running report", func() bool { return len(h.fake.reportsFor(task.id)) >= 2 })
	eventually(t, time.Second, "further polls", settled(h, 5))
	run.stop(t)

	if got := stages(h.fake.allReports()); got != "received running" {
		t.Errorf("reports %q; a 409 must not be sent again", got)
	}
	if len(setup.hook.calls()) != 0 || probe.count() != 1 {
		t.Errorf("a task the platform withdrew was acted on: %d hook calls, %d check runs", len(setup.hook.calls()), probe.count())
	}
}

func TestRecoveryDoesNotActWhenTheReceiptCannotBeDelivered(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	h.fake.setRule(pathTaskResult, dropConnection, -1)
	options := recoveryOptions()
	options.TaskReportWindow = 100 * time.Millisecond
	chk, _ := quietCheck(check.Up)
	task := setup.sign(t, nil)
	h.offer(task)
	run := h.start(options, chk)

	eventually(t, 3*time.Second, "the receipt to be given up", func() bool { return h.logs.Count("did not accept a recovery task report") >= 1 })
	if attempts := len(h.fake.requestsTo(pathTaskResult)); attempts < 2 {
		t.Errorf("the receipt was tried %d times, want retries with backoff", attempts)
	}
	h.fake.clearRules()
	eventually(t, 3*time.Second, "the task offered again to be refused as replayed", func() bool { return h.fake.taskStatus(task.id) == "refused" })
	run.stop(t)

	if got := stages(h.fake.reportsFor(task.id)); got != "refused:replayed" {
		t.Errorf("reports that reached the platform: %q", got)
	}
	if len(setup.hook.calls()) != 0 {
		t.Error("the hook was called for a task the platform never saw received")
	}
}

func TestRecoveryFailedHookIsReportedAndNotVerified(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	setup.hook.status = http.StatusFound
	chk, probe := quietCheck(check.Down)
	task := setup.sign(t, nil)
	h.offer(task)
	run := h.start(recoveryOptions(), chk)

	eventually(t, 3*time.Second, "the task to fail", func() bool { return h.fake.taskStatus(task.id) == "failed" })
	eventually(t, time.Second, "further polls", settled(h, 5))
	run.stop(t)

	reports := h.fake.reportsFor(task.id)
	if stages(reports) != "received running failed:hook_status" {
		t.Fatalf("reports %q", stages(reports))
	}
	if evidence := reports[2].Evidence; evidence == nil || evidence.HTTPStatus != http.StatusFound || evidence.Outcome != "hook answered 302" {
		t.Errorf("failed evidence: %+v", evidence)
	}
	if probe.count() != 1 || len(h.fake.eventsFor(checkName)) != 0 {
		t.Errorf("the verification check ran after a failed action: %d check runs, %d events", probe.count(), len(h.fake.eventsFor(checkName)))
	}
}

func TestRecoveryVerifiesOnlyAfterTheFinishedReportIsAccepted(t *testing.T) {
	cases := map[string]func(h *harness){
		"the platform refuses the finished report": func(h *harness) {
			h.fake.with(func(fake *fakePlatform) { fake.recovery.conflictOn = api.StageFinished })
		},
		"the finished report never arrives": func(h *harness) {
			h.fake.with(func(fake *fakePlatform) { fake.recovery.dropStage = api.StageFinished })
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			setup := h.enableRecovery()
			arrange(h)
			options := recoveryOptions()
			options.TaskReportWindow = 100 * time.Millisecond
			chk, probe := quietCheck(check.Up)
			task := setup.sign(t, nil)
			h.offer(task)
			run := h.start(options, chk)

			eventually(t, 3*time.Second, "the finished report to be given up", func() bool { return h.logs.Count("did not accept a recovery task report") == 1 })
			eventually(t, time.Second, "further polls", settled(h, 5))
			run.stop(t)

			if calls := len(setup.hook.calls()); calls != 1 {
				t.Fatalf("the hook was called %d times", calls)
			}
			if h.fake.taskStatus(task.id) != "running" {
				t.Errorf("the platform holds the task as %q", h.fake.taskStatus(task.id))
			}
			if probe.count() != 1 || len(h.fake.eventsFor(checkName)) != 0 {
				t.Errorf("the verification check ran without an accepted finished report: %d check runs, %d events", probe.count(), len(h.fake.eventsFor(checkName)))
			}
		})
	}
}

func TestRecoveryVerifiesAfterAFinishedReportWhoseAnswerWasLost(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	h.fake.with(func(fake *fakePlatform) { fake.recovery.loseAnswerTo = api.StageFinished })
	chk, _ := quietCheck(check.Up)
	task := setup.sign(t, nil)
	h.offer(task)
	run := h.start(recoveryOptions(), chk)

	eventually(t, 3*time.Second, "the verification event", func() bool { return len(h.fake.eventsFor(checkName)) == 1 })
	run.stop(t)

	reports := h.fake.reportsFor(task.id)
	if stages(reports) != "received running finished finished" {
		t.Fatalf("reports %q, want the finished report sent again after its answer was lost", stages(reports))
	}
	verification := h.fake.eventsFor(checkName)[0]
	if recorded := reports[2].at.Truncate(time.Millisecond); verification.OccurredAt.Before(recorded) {
		t.Errorf("the verification check ran at %s, before the platform recorded finished at %s", verification.OccurredAt, recorded)
	}
	if calls := len(setup.hook.calls()); calls != 1 {
		t.Errorf("the hook was called %d times", calls)
	}
}

func TestRecoveryWithAnUnknownVerificationCheckStillReports(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	chk, _ := quietCheck(check.Up)
	task := setup.sign(t, set("verification_check", "web-01:gone"))
	h.offer(task)
	run := h.start(recoveryOptions(), chk)

	eventually(t, 3*time.Second, "the task to finish", func() bool { return h.fake.taskStatus(task.id) == "finished" })
	eventually(t, time.Second, "the warning", func() bool { return h.logs.Count("could not run the verification check") == 1 })
	run.stop(t)

	if len(h.fake.eventsFor(checkName)) != 0 {
		t.Error("another check was run in place of the verification check")
	}
}

func TestRecoveryStopsCleanlyDuringAnAction(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	setup.hook.hold = make(chan struct{})
	t.Cleanup(func() { close(setup.hook.hold) })
	chk, _ := quietCheck(check.Up)
	task := setup.sign(t, nil)
	h.offer(task)
	run := h.start(recoveryOptions(), chk)

	eventually(t, 3*time.Second, "the hook to be called", func() bool { return len(setup.hook.calls()) == 1 })
	if elapsed := run.stop(t); elapsed > 2*time.Second {
		t.Errorf("shutdown during an action took %s", elapsed)
	}

	reports := h.fake.reportsFor(task.id)
	if stages(reports) != "received running failed:hook_error" {
		t.Fatalf("reports %q", stages(reports))
	}
	if evidence := reports[2].Evidence; evidence == nil || evidence.Outcome != "agent stopped during the action" {
		t.Errorf("failed evidence: %+v", evidence)
	}
	if len(h.fake.eventsFor(checkName)) != 0 {
		t.Error("the verification check ran after shutdown began")
	}
}

func TestRunNowSavesBehindEventsAlreadyWaiting(t *testing.T) {
	h := newHarness(t)
	setup := h.enableRecovery()
	options := recoveryOptions()
	options.Confirm = 1
	counted, probe := newCheck(3)
	counted.Name = "web-01:worker"
	chk, steady := quietCheck(check.Up)
	run := h.start(options, counted, chk)

	eventually(t, 2*time.Second, "four scheduled results", func() bool { return probe.finished() && steady.count() == 1 })
	if stored := len(h.fake.counters()); stored != 0 {
		t.Fatalf("%d scheduled events were sent before the flush timer", stored)
	}

	task := setup.sign(t, nil)
	h.offer(task)
	eventually(t, 3*time.Second, "the scheduled and the on-demand event of the verification check", func() bool { return len(h.fake.eventsFor(checkName)) == 2 })
	run.stop(t)

	h.fake.mu.Lock()
	var names []string
	for _, stored := range h.fake.events {
		names = append(names, stored.Body.Name)
	}
	h.fake.mu.Unlock()
	slices.Sort(names[:len(names)-1])
	if got := strings.Join(names, " "); got != checkName+" web-01:worker web-01:worker web-01:worker "+checkName {
		t.Errorf("events arrived as %q; the on-demand event must follow the four that were already waiting", got)
	}
}
