package recovery

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/alimrun/autoheal-agent/internal/api"
	"github.com/alimrun/autoheal-agent/internal/check"
)

const maxHookAnswerBytes = 64 << 10

var errNotLoopback = errors.New("cache hooks may only reach a loopback address")

// CheckRunner runs one of the agent's checks on demand and spools its result at once.
type CheckRunner interface {
	RunNow(ctx context.Context, name string) (check.Status, error)
}

// Outcome is how an action ended. Failure is empty when it finished.
type Outcome struct {
	Failure  string
	Evidence api.TaskEvidence
}

// Registry holds the only two actions the agent can run.
type Registry struct {
	policy    Policy
	client    *http.Client
	userAgent string
}

func NewRegistry(policy Policy, roots *x509.CertPool, userAgent string) *Registry {
	return &Registry{policy: policy, client: newHookClient(roots), userAgent: userAgent}
}

// newHookClient reaches loopback addresses only, through no proxy, and never follows a redirect.
func newHookClient(roots *x509.CertPool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: loopbackOnly}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
			TLSHandshakeTimeout: 5 * time.Second,
			DisableKeepAlives:   true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// loopbackOnly runs on the resolved address just before connecting, so a name that resolves
// outside the host is refused too.
func loopbackOnly(_, address string, _ syscall.RawConn) error {
	resolved, err := netip.ParseAddrPort(address)
	if err != nil || !resolved.Addr().Unmap().IsLoopback() {
		return errNotLoopback
	}
	return nil
}

// Run runs a verified task's action once and returns bounded evidence about it.
func (registry *Registry) Run(ctx context.Context, task *Task, checks CheckRunner) Outcome {
	started := time.Now()
	var outcome Outcome
	if task.Action == ActionClearCache {
		outcome = registry.clearCache(ctx, task)
	} else {
		outcome = registry.recheckHealth(ctx, task, checks)
	}
	duration := time.Since(started).Milliseconds()
	outcome.Evidence.DurationMs = &duration
	return outcome
}

func (registry *Registry) recheckHealth(ctx context.Context, task *Task, checks CheckRunner) Outcome {
	if task.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, task.Timeout)
		defer cancel()
	}

	status, err := checks.RunNow(ctx, task.Target)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return failed(FailureTimeout, "check timed out")
	case err != nil:
		return failed(FailureCheckError, "check could not be run")
	}
	return Outcome{Evidence: api.TaskEvidence{Outcome: "check is " + string(status), CheckStatus: string(status)}}
}

func (registry *Registry) clearCache(ctx context.Context, task *Task) Outcome {
	hook, _ := registry.policy.hook(task.Target)
	timeout := hook.Timeout
	if task.Timeout > 0 && task.Timeout < timeout {
		timeout = task.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, _ := json.Marshal(struct {
		TaskID string `json:"task_id"`
	}{task.ID})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.URL, bytes.NewReader(body))
	if err != nil {
		return failed(FailureHookError, "hook address is not valid")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", registry.userAgent)
	if hook.TokenFile != "" {
		token, err := os.ReadFile(hook.TokenFile)
		if err != nil {
			return failed(FailureHookError, "hook token file could not be read")
		}
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}

	response, err := registry.client.Do(request)
	if err != nil {
		var netError net.Error
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &netError) && netError.Timeout()) {
			return failed(FailureTimeout, "hook timed out")
		}
		return failed(FailureHookError, "hook could not be reached")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxHookAnswerBytes))
	response.Body.Close()

	evidence := api.TaskEvidence{Outcome: fmt.Sprintf("hook answered %d", response.StatusCode), HTTPStatus: response.StatusCode}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return Outcome{Failure: FailureHookStatus, Evidence: evidence}
	}
	return Outcome{Evidence: evidence}
}

func failed(code, outcome string) Outcome {
	return Outcome{Failure: code, Evidence: api.TaskEvidence{Outcome: outcome}}
}
