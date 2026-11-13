package recovery

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alimrun/autoheal-agent/internal/check"
)

const testUserAgent = "autoheal-agent/test recovery"

type hookCall struct {
	method        string
	path          string
	body          string
	contentType   string
	authorization string
	userAgent     string
}

// hookServer records the calls a cache hook receives and answers each with status.
func hookServer(t *testing.T, status int) (*httptest.Server, *[]hookCall) {
	t.Helper()
	var calls []hookCall
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		calls = append(calls, hookCall{
			method:        request.Method,
			path:          request.URL.Path,
			body:          string(body),
			contentType:   request.Header.Get("Content-Type"),
			authorization: request.Header.Get("Authorization"),
			userAgent:     request.Header.Get("User-Agent"),
		})
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(`{"cleared":["views"],"internal":"must never leave the host"}`))
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func clearCacheTask() *Task {
	return &Task{ID: testTaskID, Nonce: testNonce, Action: ActionClearCache, Target: "views", Timeout: time.Minute}
}

func registryFor(hook Hook) *Registry {
	return NewRegistry(Policy{Prefix: "web-01", Rechecks: []string{"api"}, Hooks: []Hook{hook}}, nil, testUserAgent)
}

func TestHookAnswering204Finishes(t *testing.T) {
	server, calls := hookServer(t, http.StatusNoContent)
	registry := registryFor(Hook{Name: "views", URL: server.URL + "/internal/cache/clear", Timeout: 5 * time.Second})

	outcome := registry.Run(context.Background(), clearCacheTask(), nil)

	if outcome.Failure != "" {
		t.Fatalf("failed with %s", outcome.Failure)
	}
	evidence := outcome.Evidence
	if evidence.HTTPStatus != 204 || evidence.Outcome != "hook answered 204" || evidence.CheckStatus != "" || evidence.DurationMs == nil || *evidence.DurationMs < 0 {
		t.Errorf("evidence: %+v", evidence)
	}
	want := hookCall{
		method: http.MethodPost, path: "/internal/cache/clear", body: `{"task_id":"` + testTaskID + `"}`,
		contentType: "application/json", userAgent: testUserAgent,
	}
	if len(*calls) != 1 || (*calls)[0] != want {
		t.Errorf("the hook should get exactly one POST with only the task ID:\n got %+v\nwant %+v", *calls, want)
	}
}

func TestHookAnswerBodyNeverReachesTheEvidence(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusInternalServerError} {
		server, _ := hookServer(t, status)
		outcome := registryFor(Hook{Name: "views", URL: server.URL, Timeout: 5 * time.Second}).Run(context.Background(), clearCacheTask(), nil)
		if strings.Contains(outcome.Evidence.Outcome, "internal") || len(outcome.Evidence.Outcome) > 64 {
			t.Errorf("status %d: outcome %q", status, outcome.Evidence.Outcome)
		}
	}
}

func TestHookStatusOutside2xxFails(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusInternalServerError} {
		server, calls := hookServer(t, status)
		outcome := registryFor(Hook{Name: "views", URL: server.URL, Timeout: 5 * time.Second}).Run(context.Background(), clearCacheTask(), nil)
		if outcome.Failure != FailureHookStatus || outcome.Evidence.HTTPStatus != status || len(*calls) != 1 {
			t.Errorf("status %d: %+v after %d calls", status, outcome, len(*calls))
		}
	}
}

func TestHookRedirectIsNotFollowed(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		targetHits.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/elsewhere", http.StatusFound)
	}))
	defer origin.Close()

	tokenFile := filepath.Join(t.TempDir(), "hook-token")
	if err := os.WriteFile(tokenFile, []byte("test-hook-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outcome := registryFor(Hook{Name: "views", URL: origin.URL, TokenFile: tokenFile, Timeout: 5 * time.Second}).Run(context.Background(), clearCacheTask(), nil)

	if outcome.Failure != FailureHookStatus || outcome.Evidence.HTTPStatus != http.StatusFound || outcome.Evidence.Outcome != "hook answered 302" {
		t.Errorf("a redirect must fail as hook_status: %+v", outcome)
	}
	if targetHits.Load() != 0 {
		t.Error("the redirect target was contacted, so the hook token could have leaked")
	}
}

func TestHookOutsideLoopbackIsRefusedBeforeConnecting(t *testing.T) {
	registry := registryFor(Hook{Name: "views", URL: "http://192.0.2.10:8080/cache/clear", Timeout: 5 * time.Second})

	started := time.Now()
	outcome := registry.Run(context.Background(), clearCacheTask(), nil)

	if outcome.Failure != FailureHookError || outcome.Evidence.HTTPStatus != 0 || outcome.Evidence.Outcome != "hook could not be reached" {
		t.Errorf("outcome: %+v", outcome)
	}
	if strings.Contains(outcome.Evidence.Outcome, "192.0.2.10") {
		t.Error("the evidence must not hold the hook address")
	}
	if time.Since(started) > 2*time.Second {
		t.Error("the address was dialled instead of being refused")
	}

	_, err := registry.client.Get("http://192.0.2.10:8080/cache/clear")
	if !errors.Is(err, errNotLoopback) {
		t.Errorf("the dialer should refuse the address itself, got %v", err)
	}
}

func TestLoopbackOnly(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080":          true,
		"127.200.3.4:80":          true,
		"[::1]:8080":              true,
		"[::ffff:127.0.0.1]:8080": true,
		"10.0.0.5:80":             false,
		"192.168.1.10:8080":       false,
		"169.254.169.254:80":      false,
		"0.0.0.0:8080":            false,
		"[::]:8080":               false,
		"[2001:db8::1]:443":       false,
		"[fe80::1%lo0]:80":        false,
		"localhost:80":            false,
		"":                        false,
	}
	for address, allowed := range cases {
		err := loopbackOnly("tcp", address, nil)
		if (err == nil) != allowed {
			t.Errorf("loopbackOnly(%q) = %v, want allowed %v", address, err, allowed)
		}
	}
}

func TestHookTimeout(t *testing.T) {
	released := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-released:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(released) })

	cases := map[string]struct {
		hookTimeout time.Duration
		taskTimeout time.Duration
	}{
		"the hook's own timeout":            {50 * time.Millisecond, time.Minute},
		"a shorter timeout from the task":   {time.Minute, 50 * time.Millisecond},
		"a task that names no timeout":      {50 * time.Millisecond, 0},
		"a longer timeout from the task":    {50 * time.Millisecond, time.Hour},
		"a negative timeout from the task":  {50 * time.Millisecond, -time.Second},
		"both the same, the action stops":   {50 * time.Millisecond, 50 * time.Millisecond},
		"task timeout below a long default": {2 * time.Minute, 60 * time.Millisecond},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			task := clearCacheTask()
			task.Timeout = testCase.taskTimeout
			started := time.Now()
			outcome := registryFor(Hook{Name: "views", URL: server.URL, Timeout: testCase.hookTimeout}).Run(context.Background(), task, nil)

			if outcome.Failure != FailureTimeout || outcome.Evidence.Outcome != "hook timed out" || outcome.Evidence.HTTPStatus != 0 {
				t.Errorf("outcome: %+v", outcome)
			}
			if waited := time.Since(started); waited > 5*time.Second {
				t.Errorf("the action ran for %s", waited)
			}
		})
	}
}

func TestHookSendsTheTokenFromItsFile(t *testing.T) {
	server, calls := hookServer(t, http.StatusOK)
	tokenFile := filepath.Join(t.TempDir(), "hook-token")
	if err := os.WriteFile(tokenFile, []byte("  test-hook-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	outcome := registryFor(Hook{Name: "views", URL: server.URL, TokenFile: tokenFile, Timeout: 5 * time.Second}).Run(context.Background(), clearCacheTask(), nil)

	if outcome.Failure != "" || len(*calls) != 1 || (*calls)[0].authorization != "Bearer test-hook-token" {
		t.Errorf("outcome %+v, calls %+v", outcome, *calls)
	}
	if strings.Contains(outcome.Evidence.Outcome, "test-hook-token") {
		t.Error("the evidence must not hold the token")
	}
}

func TestHookWithAnUnreadableTokenFileIsNotCalled(t *testing.T) {
	server, calls := hookServer(t, http.StatusOK)
	hook := Hook{Name: "views", URL: server.URL, TokenFile: filepath.Join(t.TempDir(), "absent"), Timeout: 5 * time.Second}

	outcome := registryFor(hook).Run(context.Background(), clearCacheTask(), nil)

	if outcome.Failure != FailureHookError || outcome.Evidence.Outcome != "hook token file could not be read" || len(*calls) != 0 {
		t.Errorf("outcome %+v after %d calls", outcome, len(*calls))
	}
}

func TestHookThatIsDownFails(t *testing.T) {
	server, _ := hookServer(t, http.StatusOK)
	address := server.URL
	server.Close()

	outcome := registryFor(Hook{Name: "views", URL: address, Timeout: 5 * time.Second}).Run(context.Background(), clearCacheTask(), nil)

	if outcome.Failure != FailureHookError || strings.Contains(outcome.Evidence.Outcome, "127.0.0.1") {
		t.Errorf("outcome: %+v", outcome)
	}
}

type fakeChecks struct {
	status check.Status
	err    error
	wait   time.Duration
	names  []string
}

func (checks *fakeChecks) RunNow(ctx context.Context, name string) (check.Status, error) {
	checks.names = append(checks.names, name)
	if checks.wait > 0 {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(checks.wait):
		}
	}
	return checks.status, checks.err
}

func recheckTask() *Task {
	return &Task{ID: testTaskID, Nonce: testNonce, Action: ActionRecheckHealth, Target: "web-01:api", Timeout: time.Minute}
}

func TestRecheckHealthRunsTheNamedCheckOnce(t *testing.T) {
	for _, status := range []check.Status{check.Up, check.Degraded, check.Down} {
		checks := &fakeChecks{status: status}
		outcome := registryFor(Hook{Name: "views"}).Run(context.Background(), recheckTask(), checks)

		if outcome.Failure != "" || outcome.Evidence.CheckStatus != string(status) || outcome.Evidence.Outcome != "check is "+string(status) || outcome.Evidence.HTTPStatus != 0 {
			t.Errorf("%s: %+v", status, outcome)
		}
		if len(checks.names) != 1 || checks.names[0] != "web-01:api" {
			t.Errorf("%s: checks run: %v", status, checks.names)
		}
	}
}

func TestRecheckHealthFailures(t *testing.T) {
	failing := &fakeChecks{err: errors.New("nothing to report yet")}
	outcome := registryFor(Hook{Name: "views"}).Run(context.Background(), recheckTask(), failing)
	if outcome.Failure != FailureCheckError || outcome.Evidence.CheckStatus != "" || outcome.Evidence.Outcome != "check could not be run" {
		t.Errorf("check error: %+v", outcome)
	}

	slow := &fakeChecks{status: check.Up, wait: time.Minute}
	task := recheckTask()
	task.Timeout = 30 * time.Millisecond
	outcome = registryFor(Hook{Name: "views"}).Run(context.Background(), task, slow)
	if outcome.Failure != FailureTimeout || outcome.Evidence.Outcome != "check timed out" {
		t.Errorf("timeout: %+v", outcome)
	}
}

func TestPolicyActions(t *testing.T) {
	cases := map[string]Policy{
		"":                           {},
		"recheck_health":             {Rechecks: []string{"api"}},
		"clear_cache":                {Hooks: []Hook{{Name: "views"}}},
		"recheck_health,clear_cache": {Rechecks: []string{"api"}, Hooks: []Hook{{Name: "views"}}},
	}
	for want, policy := range cases {
		if got := strings.Join(policy.Actions(), ","); got != want {
			t.Errorf("actions %q, want %q", got, want)
		}
	}
}
