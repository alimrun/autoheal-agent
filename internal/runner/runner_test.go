package runner_test

// Integration tests for the runner against a recording fake platform (design decision 19).
// Short real timings stand in for a fake clock: every duration lives in runner.Options.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alimrun/autoheal-agent/internal/api"
	"github.com/alimrun/autoheal-agent/internal/check"
	"github.com/alimrun/autoheal-agent/internal/credential"
	"github.com/alimrun/autoheal-agent/internal/event"
	"github.com/alimrun/autoheal-agent/internal/runner"
	"github.com/alimrun/autoheal-agent/internal/spool"
)

const (
	pathHeartbeat = "/api/v1/agent/heartbeat"
	pathRotate    = "/api/v1/agent/credential/rotate"
	pathTelemetry = "/api/v1/agent/telemetry/events"
	allPaths      = "*"

	initialToken = "token-0"
	checkName    = "web-01:api"
)

func testOptions() runner.Options {
	return runner.Options{
		BatchSize:         50,
		FlushInterval:     20 * time.Millisecond,
		BackoffBase:       2 * time.Millisecond,
		BackoffMax:        20 * time.Millisecond,
		CredentialRecheck: 100 * time.Millisecond,
		ShutdownSend:      300 * time.Millisecond,
		MinHeartbeat:      10 * time.Millisecond,
		MaxHeartbeat:      20 * time.Millisecond,
		MaxAge:            time.Hour,
		SkewLimit:         2 * time.Minute,
		WarnEvery:         time.Hour,
		Confirm:           1,
		Refresh:           0,
		QueueSize:         500,
		CheckJitter:       0,
	}
}

// ---------------------------------------------------------------------------
// Fake platform

type mode int

const (
	answerNormally   mode = iota
	dropConnection        // hijack and close before doing anything: network loss
	storeThenDrop         // do the work, then close instead of answering: a lost answer
	refuseCredential      // 401 agent_unauthenticated_401
	refuseBatch           // 422 validation_failed_422 with field errors
	rateLimit             // 429 http_429 with Retry-After
	hang                  // never answer; wait for the client to give up
)

type rule struct {
	mode      mode
	remaining int // negative: until cleared
}

type request struct {
	path     string
	token    string
	at       time.Time
	ids      []string
	counters []int
	status   int  // 0 when the connection was dropped or never answered
	lost     bool // the work was done but the answer was dropped
}

type rotation struct{ oldToken, newToken string }

type sentEvent struct {
	EventID string `json:"event_id"`
	Message string `json:"message"`
}

type reply struct {
	status     int
	code       string
	message    string
	content    any
	errors     any
	retryAfter string
}

type fakePlatform struct {
	server   *httptest.Server
	closing  chan struct{}
	first401 chan struct{}
	once401  sync.Once

	mu                 sync.Mutex
	rules              map[string]*rule
	valid              map[string]bool
	stateDir           string
	heartbeatInterval  int
	minimumVersion     string
	clockOffset        time.Duration
	retryAfter         string
	rotateWanted       bool
	revokeOnRotate     bool
	beforeRotateAnswer func()
	issued             int
	latest             string
	issuedTokens       map[string]bool
	savedBeforeUse     map[string]bool
	rotations          []rotation
	requests           []request
	stored             map[string]string // event_id -> message
	duplicates         int
}

func newFakePlatform(t *testing.T) *fakePlatform {
	fake := &fakePlatform{
		closing:           make(chan struct{}),
		first401:          make(chan struct{}),
		rules:             map[string]*rule{},
		valid:             map[string]bool{},
		heartbeatInterval: 1,
		minimumVersion:    "0.1.0",
		issuedTokens:      map[string]bool{},
		savedBeforeUse:    map[string]bool{},
		stored:            map[string]string{},
	}
	fake.server = httptest.NewServer(fake)
	t.Cleanup(func() {
		close(fake.closing)
		fake.server.Close()
	})
	return fake
}

func (fake *fakePlatform) with(change func(*fakePlatform)) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	change(fake)
}

func (fake *fakePlatform) setRule(path string, next mode, times int) {
	fake.with(func(fake *fakePlatform) { fake.rules[path] = &rule{mode: next, remaining: times} })
}

func (fake *fakePlatform) clearRules() {
	fake.with(func(fake *fakePlatform) { fake.rules = map[string]*rule{} })
}

// take is called with mu held.
func (fake *fakePlatform) take(path string) mode {
	for _, key := range []string{path, allPaths} {
		current, found := fake.rules[key]
		if !found {
			continue
		}
		if current.remaining > 0 {
			current.remaining--
			if current.remaining == 0 {
				delete(fake.rules, key)
			}
		}
		return current.mode
	}
	return answerNormally
}

// noteUse is called with mu held. The first time a rotated token is used, the credential
// file must already hold it.
func (fake *fakePlatform) noteUse(token string) {
	if token != "" && token == fake.latest {
		fake.rotateWanted = false
	}
	if !fake.issuedTokens[token] {
		return
	}
	if _, seen := fake.savedBeforeUse[token]; seen {
		return
	}
	saved, _, err := credential.Load(fake.stateDir)
	fake.savedBeforeUse[token] = err == nil && saved.Token == token
}

func (fake *fakePlatform) ServeHTTP(writer http.ResponseWriter, incoming *http.Request) {
	body, _ := io.ReadAll(incoming.Body)
	token := strings.TrimPrefix(incoming.Header.Get("Authorization"), "Bearer ")
	path := incoming.URL.Path

	var payload struct {
		Events []sentEvent `json:"events"`
	}
	if path == pathTelemetry {
		_ = json.Unmarshal(body, &payload)
	}

	fake.mu.Lock()
	current := fake.take(path)
	fake.noteUse(token)
	record := request{path: path, token: token, at: time.Now()}
	for _, sent := range payload.Events {
		record.ids = append(record.ids, sent.EventID)
		record.counters = append(record.counters, counterOf(sent.Message))
	}

	var answer reply
	switch {
	case current == dropConnection || current == hang:
	case current == refuseCredential || !fake.valid[token]:
		answer = reply{status: http.StatusUnauthorized, code: "agent_unauthenticated_401", message: "Unauthenticated."}
	case current == refuseBatch:
		answer = reply{
			status: http.StatusUnprocessableEntity, code: "validation_failed_422", message: "The given data was invalid.",
			errors: map[string][]string{"events.0.occurred_at": {"The occurred at must be a date before or equal to now."}},
		}
	case current == rateLimit:
		answer = reply{status: http.StatusTooManyRequests, code: "http_429", message: "Too Many Attempts.", retryAfter: fake.retryAfter}
	default:
		answer = fake.answer(path, token, payload.Events)
	}
	record.status = answer.status
	if current == storeThenDrop {
		record.status, record.lost = 0, true
	}
	fake.requests = append(fake.requests, record)
	var hook func()
	if path == pathRotate && answer.status == http.StatusCreated {
		hook = fake.beforeRotateAnswer
	}
	offset := fake.clockOffset
	fake.mu.Unlock()

	if answer.status == http.StatusUnauthorized && path == pathTelemetry {
		fake.once401.Do(func() { close(fake.first401) })
	}

	switch current {
	case dropConnection, storeThenDrop:
		if conn, _, err := http.NewResponseController(writer).Hijack(); err == nil {
			conn.Close()
		}
		return
	case hang:
		select {
		case <-incoming.Context().Done():
		case <-fake.closing:
		}
		return
	}
	if hook != nil {
		hook()
	}

	if offset != 0 {
		writer.Header().Set("Date", time.Now().Add(offset).UTC().Format(http.TimeFormat))
	}
	if answer.retryAfter != "" {
		writer.Header().Set("Retry-After", answer.retryAfter)
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(answer.status)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"identical_code": answer.code,
		"message":        answer.message,
		"content":        answer.content,
		"errors":         answer.errors,
	})
}

// answer is called with mu held, for an authenticated request.
func (fake *fakePlatform) answer(path, token string, events []sentEvent) reply {
	expires := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	switch path {
	case pathHeartbeat:
		return reply{status: http.StatusOK, code: "agent_heartbeat_200", message: "Heartbeat recorded.", content: map[string]any{
			"heartbeat_interval": fake.heartbeatInterval,
			"credential":         map[string]any{"expires_at": expires, "rotate": fake.rotateWanted},
			"minimum_version":    fake.minimumVersion,
		}}
	case pathRotate:
		fake.issued++
		issued := fmt.Sprintf("token-%d", fake.issued)
		fake.valid[issued] = true
		fake.issuedTokens[issued] = true
		if fake.revokeOnRotate {
			delete(fake.valid, token)
		}
		fake.latest = issued
		fake.rotations = append(fake.rotations, rotation{oldToken: token, newToken: issued})
		return reply{status: http.StatusCreated, code: "agent_credential_rotated_201", message: "Credential rotated.", content: map[string]any{
			"credential": map[string]any{"token": issued, "expires_at": expires, "scopes": []string{"telemetry:write"}},
		}}
	case pathTelemetry:
		accepted, duplicates := 0, 0
		for _, sent := range events {
			if _, seen := fake.stored[sent.EventID]; seen {
				duplicates++
				continue
			}
			fake.stored[sent.EventID] = sent.Message
			accepted++
		}
		fake.duplicates += duplicates
		return reply{status: http.StatusAccepted, code: "telemetry_accepted_202", message: "Telemetry accepted.", content: map[string]any{
			"accepted": accepted, "duplicates": duplicates,
		}}
	}
	return reply{status: http.StatusNotFound, code: "http_404", message: "Not Found."}
}

func (fake *fakePlatform) requestsTo(path string) []request {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var matching []request
	for _, seen := range fake.requests {
		if path == allPaths || seen.path == path {
			matching = append(matching, seen)
		}
	}
	return matching
}

func (fake *fakePlatform) counters() []int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	counters := make([]int, 0, len(fake.stored))
	for _, message := range fake.stored {
		counters = append(counters, counterOf(message))
	}
	slices.Sort(counters)
	return counters
}

func (fake *fakePlatform) storedIDs() map[string]bool {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	ids := map[string]bool{}
	for id := range fake.stored {
		ids[id] = true
	}
	return ids
}

func (fake *fakePlatform) duplicateCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.duplicates
}

func countWhere(requests []request, keep func(request) bool) int {
	count := 0
	for _, seen := range requests {
		if keep(seen) {
			count++
		}
	}
	return count
}

// counterOf reads the run counter from a message such as "web-01:api is up: run 7".
func counterOf(message string) int {
	index := strings.LastIndex(message, "run ")
	if index < 0 {
		return -1
	}
	counter, err := strconv.Atoi(message[index+len("run "):])
	if err != nil {
		return -1
	}
	return counter
}

// ---------------------------------------------------------------------------
// Agent harness

type logBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (logs *logBuffer) Write(content []byte) (int, error) {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	return logs.buffer.Write(content)
}

func (logs *logBuffer) String() string {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	return logs.buffer.String()
}

func (logs *logBuffer) Count(text string) int {
	return strings.Count(logs.String(), text)
}

// countingProbe reports "up" with a run counter in its detail, limit times, then skips.
type countingProbe struct {
	mu    sync.Mutex
	limit int
	runs  int
	calls int
}

func (probe *countingProbe) Run(context.Context) check.Result {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	probe.calls++
	if probe.runs >= probe.limit {
		return check.Result{Skip: true}
	}
	probe.runs++
	return check.Result{Status: check.Up, Detail: fmt.Sprintf("run %d", probe.runs)}
}

// finished is true once every counted run has been handed to the runner: the call after the
// last counted run happens only after that run's report.
func (probe *countingProbe) finished() bool {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	return probe.calls > probe.limit
}

func newCheck(limit int) (*check.Check, *countingProbe) {
	probe := &countingProbe{limit: limit}
	return &check.Check{
		Name:         checkName,
		Kind:         check.KindTCP,
		Target:       "127.0.0.1:5432",
		Interval:     10 * time.Millisecond,
		Timeout:      time.Second,
		DownSeverity: "critical",
		Probe:        probe,
	}, probe
}

type harness struct {
	t        *testing.T
	fake     *fakePlatform
	stateDir string
	spoolDir string
	logs     *logBuffer
	log      *slog.Logger
	client   *api.Client
	details  api.Details
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := newFakePlatform(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	_, err := credential.Save(stateDir, &credential.Credential{
		APIURL:      fake.server.URL + "/api/v1",
		Agent:       credential.Ref{ID: "agent-1", Name: "web-01"},
		Project:     credential.Ref{ID: "project-1", Name: "Shop"},
		Environment: credential.Ref{ID: "environment-1", Name: "production"},
		Token:       initialToken,
		ExpiresAt:   time.Now().Add(30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("saving the credential: %v", err)
	}
	fake.with(func(fake *fakePlatform) {
		fake.stateDir = stateDir
		fake.valid[initialToken] = true
	})

	client, err := api.NewClient(fake.server.URL, api.Options{UserAgent: "autoheal-agent/0.1.0 (test)"})
	if err != nil {
		t.Fatalf("creating the client: %v", err)
	}

	logs := &logBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("agent log:\n%s", logs)
		}
	})
	return &harness{
		t:        t,
		fake:     fake,
		stateDir: stateDir,
		spoolDir: filepath.Join(stateDir, "spool"),
		logs:     logs,
		log:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		client:   client,
		details:  api.Details{Hostname: "web-01", Version: "0.1.0", OS: "linux", Arch: "amd64"},
	}
}

func (h *harness) openSpool() *spool.Spool {
	h.t.Helper()
	opened, err := spool.Open(h.spoolDir, 10000, 64<<20, h.log)
	if err != nil {
		h.t.Fatalf("opening the spool: %v", err)
	}
	return opened
}

type running struct {
	spool   *spool.Spool
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
	elapsed time.Duration
}

// start runs a new agent process on the harness's state folder: it reloads the credential
// and reopens the spool, as a restart does.
func (h *harness) start(options runner.Options, checks ...*check.Check) *running {
	h.t.Helper()
	loaded, stamp, err := credential.Load(h.stateDir)
	if err != nil {
		h.t.Fatalf("loading the credential: %v", err)
	}
	opened := h.openSpool()
	agent := runner.New(runner.Deps{
		Options:    options,
		Log:        h.log,
		Client:     h.client,
		StateDir:   h.stateDir,
		Details:    h.details,
		Checks:     checks,
		Spool:      opened,
		Credential: loaded,
		Stamp:      stamp,
	})

	ctx, cancel := context.WithCancel(context.Background())
	run := &running{spool: opened, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(run.done)
		agent.Run(ctx)
	}()
	h.t.Cleanup(func() { run.stop(h.t) })
	return run
}

// stop cancels the run and returns how long Run took to return.
func (run *running) stop(t *testing.T) time.Duration {
	run.once.Do(func() {
		started := time.Now()
		run.cancel()
		select {
		case <-run.done:
		case <-time.After(10 * time.Second):
			t.Errorf("Run did not return within 10 seconds of cancellation")
		}
		run.elapsed = time.Since(started)
	})
	return run.elapsed
}

func eventually(t *testing.T, within time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func sequence(from, to int) []int {
	numbers := make([]int, 0, to-from+1)
	for number := from; number <= to; number++ {
		numbers = append(numbers, number)
	}
	return numbers
}

// assertExactlyOnce fails unless got holds every counter in want exactly once, and nothing else.
func assertExactlyOnce(t *testing.T, got, want []int) {
	t.Helper()
	seen := map[int]int{}
	for _, counter := range got {
		seen[counter]++
	}
	var duplicated, missing, unexpected []int
	for counter, times := range seen {
		if times > 1 {
			duplicated = append(duplicated, counter)
		}
		if !slices.Contains(want, counter) {
			unexpected = append(unexpected, counter)
		}
	}
	for _, counter := range want {
		if seen[counter] == 0 {
			missing = append(missing, counter)
		}
	}
	slices.Sort(duplicated)
	slices.Sort(missing)
	slices.Sort(unexpected)
	if len(duplicated)+len(missing)+len(unexpected) > 0 {
		t.Errorf("run counters: duplicated %v, missing %v, unexpected %v", duplicated, missing, unexpected)
	}
}

func allDelivered(h *harness, run *running, probe *countingProbe, total int) func() bool {
	return func() bool {
		return probe.finished() && run.spool.Len() == 0 && len(h.fake.counters()) == total
	}
}

// ---------------------------------------------------------------------------
// Tests

func TestNetworkLossAndReturn(t *testing.T) {
	h := newHarness(t)
	options := testOptions()
	const total = 50
	chk, probe := newCheck(total)
	run := h.start(options, chk)

	eventually(t, 2*time.Second, "the first events", func() bool { return len(h.fake.counters()) >= 3 })
	outageStarted := time.Now()
	h.fake.setRule(allPaths, dropConnection, -1)
	eventually(t, 2*time.Second, "400 ms of failed sends", func() bool {
		failed := countWhere(h.fake.requestsTo(pathTelemetry), func(seen request) bool { return seen.status == 0 })
		return time.Since(outageStarted) >= 400*time.Millisecond && failed >= 3
	})
	h.fake.clearRules()
	recovered := time.Now()

	eventually(t, 3*time.Second, "every run delivered", allDelivered(h, run, probe, total))
	eventually(t, time.Second, "heartbeats to resume", func() bool {
		return countWhere(h.fake.requestsTo(pathHeartbeat), func(seen request) bool {
			return seen.status == http.StatusOK && seen.at.After(recovered)
		}) >= 2
	})

	assertExactlyOnce(t, h.fake.counters(), sequence(1, total))
	if duplicates := h.fake.duplicateCount(); duplicates != 0 {
		t.Errorf("the platform counted %d duplicates; dropped connections stored nothing", duplicates)
	}

	// Every wait between two failed attempts is one backoff wait, capped at BackoffMax.
	// An uncapped policy would reach 128 ms within the outage.
	slack := 80 * time.Millisecond
	for _, path := range []string{pathHeartbeat, pathTelemetry} {
		var previous time.Time
		var longest time.Duration
		failures := 0
		for _, seen := range h.fake.requestsTo(path) {
			if seen.status != 0 {
				previous = time.Time{}
				continue
			}
			failures++
			if !previous.IsZero() {
				longest = max(longest, seen.at.Sub(previous))
			}
			previous = seen.at
		}
		if failures < 3 {
			t.Errorf("%s: only %d failed attempts during the outage", path, failures)
		}
		if longest > options.BackoffMax+slack {
			t.Errorf("%s: a backoff wait lasted %s, over BackoffMax %s", path, longest, options.BackoffMax)
		}
	}

	firstAfter := func(path string, status int) time.Duration {
		for _, seen := range h.fake.requestsTo(path) {
			if seen.status == status && seen.at.After(recovered) {
				return seen.at.Sub(recovered)
			}
		}
		return time.Hour
	}
	if waited := firstAfter(pathHeartbeat, http.StatusOK); waited > options.BackoffMax+slack {
		t.Errorf("the first heartbeat after the network returned came %s later", waited)
	}
	if waited := firstAfter(pathTelemetry, http.StatusAccepted); waited > options.BackoffMax+options.FlushInterval+slack {
		t.Errorf("the first delivery after the network returned came %s later", waited)
	}
}

func TestLostAnswerIsResentWithTheSameIdentifiers(t *testing.T) {
	h := newHarness(t)
	h.fake.setRule(pathTelemetry, storeThenDrop, 1)
	const total = 10
	chk, probe := newCheck(total)
	run := h.start(testOptions(), chk)

	eventually(t, 2*time.Second, "every run delivered", allDelivered(h, run, probe, total))

	telemetry := h.fake.requestsTo(pathTelemetry)
	if len(telemetry) < 2 {
		t.Fatalf("got %d telemetry requests, want the lost one and its resend", len(telemetry))
	}
	lost, resent := telemetry[0], telemetry[1]
	if !lost.lost {
		t.Fatalf("the first telemetry request was not the lost answer: %+v", lost)
	}
	if !slices.Equal(lost.ids, resent.ids) {
		t.Errorf("the resend carried identifiers %v, want the lost batch's %v", resent.ids, lost.ids)
	}
	if resent.status != http.StatusAccepted {
		t.Errorf("the resend got %d, want 202", resent.status)
	}
	if duplicates := h.fake.duplicateCount(); duplicates != len(lost.ids) {
		t.Errorf("the platform counted %d duplicates, want %d", duplicates, len(lost.ids))
	}
	assertExactlyOnce(t, h.fake.counters(), sequence(1, total))
}

func TestRestartMidOutage(t *testing.T) {
	h := newHarness(t)
	options := testOptions()
	h.fake.setRule(allPaths, dropConnection, -1)
	const total = 10
	chk, probe := newCheck(total)
	first := h.start(options, chk)

	eventually(t, 2*time.Second, "every run spooled", func() bool { return probe.finished() && first.spool.Len() == total })
	first.stop(t)
	if waiting := first.spool.Len(); waiting != total {
		t.Fatalf("after stopping mid-outage the spool holds %d events, want %d", waiting, total)
	}
	if delivered := len(h.fake.counters()); delivered != 0 {
		t.Fatalf("%d events were stored during the outage", delivered)
	}

	h.fake.clearRules()
	second := h.start(options)
	eventually(t, 2*time.Second, "the spooled events delivered", func() bool {
		return second.spool.Len() == 0 && len(h.fake.counters()) == total
	})

	assertExactlyOnce(t, h.fake.counters(), sequence(1, total))
	if duplicates := h.fake.duplicateCount(); duplicates != 0 {
		t.Errorf("the platform counted %d duplicates, want every event sent once", duplicates)
	}
}

func TestRotationSavesBeforeUseAndRetriesALostAnswer(t *testing.T) {
	h := newHarness(t)
	h.fake.with(func(fake *fakePlatform) { fake.rotateWanted = true })
	h.fake.setRule(pathRotate, storeThenDrop, 1)
	const total = 15
	chk, probe := newCheck(total)
	run := h.start(testOptions(), chk)

	eventually(t, 3*time.Second, "the second rotated token in use and every run delivered", func() bool {
		used := countWhere(h.fake.requestsTo(pathHeartbeat), func(seen request) bool { return seen.token == "token-2" }) > 0
		return used && allDelivered(h, run, probe, total)()
	})

	h.fake.mu.Lock()
	rotations := slices.Clone(h.fake.rotations)
	saved := h.fake.savedBeforeUse["token-2"]
	_, firstUsed := h.fake.savedBeforeUse["token-1"]
	h.fake.mu.Unlock()

	want := []rotation{{initialToken, "token-1"}, {initialToken, "token-2"}}
	if !slices.Equal(rotations, want) {
		t.Errorf("rotations %v, want %v: a lost answer must lead to rotating again with the old token", rotations, want)
	}
	if firstUsed {
		t.Errorf("token-1, whose answer was lost, was used")
	}
	if !saved {
		t.Errorf("token-2 was used before it was saved to the credential file")
	}
	loaded, _, err := credential.Load(h.stateDir)
	if err != nil || loaded.Token != "token-2" {
		t.Errorf("the credential file holds %v (err %v), want token-2", loaded, err)
	}
	assertExactlyOnce(t, h.fake.counters(), sequence(1, total))
}

// The platform revokes the old token at rotation, and a send with it is refused before the
// rotation's answer arrives. The new token must still be used.
func TestRotationAnswerAfterTheOldTokenWasRefused(t *testing.T) {
	h := newHarness(t)
	refused := h.fake.first401
	h.fake.with(func(fake *fakePlatform) {
		fake.rotateWanted = true
		fake.revokeOnRotate = true
		fake.beforeRotateAnswer = func() {
			select {
			case <-refused:
			case <-time.After(2 * time.Second):
			}
			time.Sleep(30 * time.Millisecond)
		}
	})
	const total = 20
	chk, probe := newCheck(total)
	run := h.start(testOptions(), chk)

	eventually(t, 3*time.Second, "every run delivered with the rotated token", allDelivered(h, run, probe, total))

	if countWhere(h.fake.requestsTo(pathTelemetry), func(seen request) bool { return seen.status == http.StatusUnauthorized }) == 0 {
		t.Fatalf("no send was refused during the rotation; the scenario did not happen")
	}
	assertExactlyOnce(t, h.fake.counters(), sequence(1, total))
}

func TestRefusedCredentialStopsSendingUntilTheFileIsRewritten(t *testing.T) {
	h := newHarness(t)
	options := testOptions()
	// A heartbeat interval far longer than the re-check: resuming must not wait for it.
	options.MaxHeartbeat = 2 * time.Second
	options.CredentialRecheck = 150 * time.Millisecond
	h.fake.with(func(fake *fakePlatform) { fake.heartbeatInterval = 1 })
	const total = 60
	chk, probe := newCheck(total)
	run := h.start(options, chk)

	eventually(t, 2*time.Second, "the first events", func() bool { return len(h.fake.counters()) >= 3 })
	h.fake.with(func(fake *fakePlatform) { delete(fake.valid, initialToken) })

	var refusedAt time.Time
	eventually(t, time.Second, "a refusal", func() bool {
		for _, seen := range h.fake.requestsTo(allPaths) {
			if seen.status == http.StatusUnauthorized {
				refusedAt = seen.at
				return true
			}
		}
		return false
	})

	time.Sleep(2 * options.CredentialRecheck)
	afterRefusal := countWhere(h.fake.requestsTo(allPaths), func(seen request) bool { return seen.at.After(refusedAt) })
	if afterRefusal > 1 {
		t.Errorf("%d requests were sent after the credential was refused, want at most one already in flight", afterRefusal)
	}
	if run.spool.Len() == 0 {
		t.Errorf("the spool did not keep filling while the credential was refused")
	}

	// What `enroll --force` does: a new credential file.
	enrolled, _, err := credential.Load(h.stateDir)
	if err != nil {
		t.Fatalf("loading the credential: %v", err)
	}
	enrolled.Token = "token-enrolled"
	h.fake.with(func(fake *fakePlatform) { fake.valid["token-enrolled"] = true })
	if _, err := credential.Save(h.stateDir, enrolled); err != nil {
		t.Fatalf("rewriting the credential: %v", err)
	}
	rewritten := time.Now()

	var resumedAt time.Time
	eventually(t, 2*time.Second, "sending to resume with the new credential", func() bool {
		for _, seen := range h.fake.requestsTo(pathTelemetry) {
			if seen.token == "token-enrolled" && seen.status == http.StatusAccepted {
				resumedAt = seen.at
				return true
			}
		}
		return false
	})
	if waited := resumedAt.Sub(rewritten); waited > options.CredentialRecheck+100*time.Millisecond {
		t.Errorf("sending resumed %s after the credential was rewritten, want within CredentialRecheck (%s)", waited, options.CredentialRecheck)
	}

	eventually(t, 2*time.Second, "every run delivered", allDelivered(h, run, probe, total))
	assertExactlyOnce(t, h.fake.counters(), sequence(1, total))
	if logged := h.logs.Count("AutoHeal refused this agent's credential"); logged != 1 {
		t.Errorf("the refusal was logged %d times, want once", logged)
	}
	if !strings.Contains(h.logs.String(), "found a new credential; resuming") {
		t.Errorf("resuming was not logged")
	}
}

func TestRejectedBatchIsDroppedAndTheNextIsSent(t *testing.T) {
	h := newHarness(t)
	h.fake.setRule(pathTelemetry, refuseBatch, 1)
	const total = 20
	chk, probe := newCheck(total)
	run := h.start(testOptions(), chk)

	eventually(t, 2*time.Second, "every run delivered or rejected", func() bool {
		telemetry := h.fake.requestsTo(pathTelemetry)
		if len(telemetry) == 0 || !probe.finished() || run.spool.Len() != 0 {
			return false
		}
		return len(h.fake.counters())+len(telemetry[0].ids) == total
	})

	telemetry := h.fake.requestsTo(pathTelemetry)
	rejected := telemetry[0]
	if rejected.status != http.StatusUnprocessableEntity || len(rejected.ids) == 0 {
		t.Fatalf("the first telemetry request was not the rejected batch: %+v", rejected)
	}
	stored := h.fake.storedIDs()
	for _, id := range rejected.ids {
		if stored[id] {
			t.Errorf("event %s from the rejected batch was stored", id)
		}
	}
	for _, seen := range telemetry[1:] {
		for _, id := range seen.ids {
			if slices.Contains(rejected.ids, id) {
				t.Errorf("event %s from the rejected batch was sent again", id)
			}
		}
	}
	if countWhere(telemetry[1:], func(seen request) bool { return seen.status == http.StatusAccepted }) == 0 {
		t.Errorf("no batch was sent after the rejected one")
	}
	assertExactlyOnce(t, append(h.fake.counters(), rejected.counters...), sequence(1, total))
	if !strings.Contains(h.logs.String(), "events.0.occurred_at") {
		t.Errorf("the field errors were not logged")
	}
}

func TestRetryAfterIsHonoredWhileHeartbeatsContinue(t *testing.T) {
	h := newHarness(t)
	h.fake.with(func(fake *fakePlatform) { fake.retryAfter = "1" })
	h.fake.setRule(pathTelemetry, rateLimit, 1)
	const total = 20
	chk, probe := newCheck(total)
	run := h.start(testOptions(), chk)

	eventually(t, 4*time.Second, "every run delivered", allDelivered(h, run, probe, total))

	telemetry := h.fake.requestsTo(pathTelemetry)
	limited, next := telemetry[0], telemetry[1]
	if limited.status != http.StatusTooManyRequests {
		t.Fatalf("the first telemetry request got %d, want 429", limited.status)
	}
	if waited := next.at.Sub(limited.at); waited < time.Second {
		t.Errorf("telemetry was sent again %s after a 429 with Retry-After: 1", waited)
	}
	heartbeats := countWhere(h.fake.requestsTo(pathHeartbeat), func(seen request) bool {
		return seen.status == http.StatusOK && seen.at.After(limited.at) && seen.at.Before(next.at)
	})
	if heartbeats < 5 {
		t.Errorf("only %d heartbeats during the Retry-After wait", heartbeats)
	}
	assertExactlyOnce(t, h.fake.counters(), sequence(1, total))
}

func TestEventsOlderThanMaxAgeAreDroppedBeforeSending(t *testing.T) {
	h := newHarness(t)
	options := testOptions()
	chk, _ := newCheck(0)
	old := time.Now().Add(-2 * options.MaxAge)
	fresh := time.Now().Add(-time.Minute)
	build := func(counter int, at time.Time) event.Event {
		return chk.Event(check.Result{Status: check.Up, Detail: fmt.Sprintf("run %d", counter)}, at, "0.1.0")
	}
	allOld := []event.Event{build(1, old), build(2, old)}
	mixed := []event.Event{build(3, old), build(4, fresh), build(5, old), build(6, fresh)}
	spooled := h.openSpool()
	if err := spooled.Append(allOld); err != nil {
		t.Fatal(err)
	}
	if err := spooled.Append(mixed); err != nil {
		t.Fatal(err)
	}

	run := h.start(options)
	eventually(t, 2*time.Second, "the spool emptied", func() bool { return run.spool.Len() == 0 && len(h.fake.counters()) == 2 })

	assertExactlyOnce(t, h.fake.counters(), []int{4, 6})
	telemetry := h.fake.requestsTo(pathTelemetry)
	if len(telemetry) != 1 {
		t.Fatalf("got %d telemetry requests, want 1", len(telemetry))
	}
	if want := []string{mixed[1].EventID, mixed[3].EventID}; !slices.Equal(telemetry[0].ids, want) {
		t.Errorf("sent identifiers %v, want the fresh events' unchanged identifiers %v", telemetry[0].ids, want)
	}
	if !strings.Contains(h.logs.String(), "dropping events too old") {
		t.Errorf("dropping old events was not logged")
	}
}

func TestShutdownDeliversWaitingEvents(t *testing.T) {
	h := newHarness(t)
	options := testOptions()
	options.FlushInterval = time.Hour // keep the events in the batcher until shutdown
	options.BatchSize = 1000
	const total = 10
	chk, probe := newCheck(total)
	run := h.start(options, chk)

	eventually(t, 2*time.Second, "every run reported", probe.finished)
	if delivered := len(h.fake.counters()); delivered != 0 {
		t.Fatalf("%d events were delivered before shutdown; they should still be waiting", delivered)
	}

	if elapsed := run.stop(t); elapsed > options.ShutdownSend+500*time.Millisecond {
		t.Errorf("Run took %s to return, want within ShutdownSend (%s) plus a margin", elapsed, options.ShutdownSend)
	}
	assertExactlyOnce(t, h.fake.counters(), sequence(1, total))
	if waiting := run.spool.Len(); waiting != 0 {
		t.Errorf("%d events stayed in the spool after a successful final send", waiting)
	}
}

func TestShutdownKeepsEventsWhenThePlatformHangs(t *testing.T) {
	h := newHarness(t)
	options := testOptions()
	options.FlushInterval = time.Hour
	options.BatchSize = 1000
	const total = 10
	chk, probe := newCheck(total)
	run := h.start(options, chk)

	eventually(t, 2*time.Second, "every run reported", probe.finished)
	h.fake.setRule(allPaths, hang, -1)

	elapsed := run.stop(t)
	if elapsed > options.ShutdownSend+500*time.Millisecond {
		t.Errorf("Run took %s to return, want within ShutdownSend (%s) plus a margin", elapsed, options.ShutdownSend)
	}
	if elapsed < options.ShutdownSend/2 {
		t.Errorf("Run returned after %s; the final send should have waited about ShutdownSend (%s)", elapsed, options.ShutdownSend)
	}
	if len(h.fake.requestsTo(pathTelemetry)) == 0 {
		t.Errorf("no final send was attempted")
	}
	if delivered := len(h.fake.counters()); delivered != 0 {
		t.Errorf("%d events were stored by a platform that never answered", delivered)
	}

	reopened := h.openSpool()
	var spooled []int
	for {
		batch, err := reopened.Oldest()
		if err != nil {
			t.Fatal(err)
		}
		if batch == nil {
			break
		}
		for _, waiting := range batch.Events {
			spooled = append(spooled, counterOf(waiting.Message))
		}
		if err := reopened.Remove(batch); err != nil {
			t.Fatal(err)
		}
	}
	assertExactlyOnce(t, spooled, sequence(1, total))
}

func TestWarnings(t *testing.T) {
	cases := []struct {
		name    string
		version string
		warn    bool
	}{
		{"older than the minimum", "0.1.0", true},
		{"development build", "0.0.0-dev", true},
		{"at the minimum", "0.2.0", false},
		{"newer than the minimum", "0.3.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.details.Version = tc.version
			h.fake.with(func(fake *fakePlatform) {
				fake.minimumVersion = "0.2.0"
				fake.clockOffset = 10 * time.Minute
			})
			run := h.start(testOptions())
			eventually(t, 2*time.Second, "three heartbeats", func() bool {
				return countWhere(h.fake.requestsTo(pathHeartbeat), func(seen request) bool { return seen.status == http.StatusOK }) >= 3
			})
			run.stop(t)

			want := 0
			if tc.warn {
				want = 1
			}
			if got := h.logs.Count("older than the minimum version"); got != want {
				t.Errorf("the minimum-version warning was logged %d times over several heartbeats, want %d", got, want)
			}
			if got := h.logs.Count("clock differs from AutoHeal's"); got != 1 {
				t.Errorf("the clock-skew warning was logged %d times, want once", got)
			}
		})
	}
}
