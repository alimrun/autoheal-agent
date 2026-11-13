// Package api talks to the AutoHeal agent API (v1, telemetry event schema 1).
package api

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
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	maxAnswerBytes = 1 << 20
	apiPrefix      = "/api/v1"
	pathRegister   = "/agent/register"
	pathHeartbeat  = "/agent/heartbeat"
	pathRotate     = "/agent/credential/rotate"
	pathTelemetry  = "/agent/telemetry/events"
	pathTasks      = "/agent/tasks"
)

// ValidateBaseURL accepts HTTPS addresses, and plain HTTP only to a loopback host. The
// dashboard shows the API base ending in /api/v1; a bare site address gets it added.
func ValidateBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, fmt.Errorf("%q is not an http or https address", raw)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("the API address must not contain a user name, password, query or fragment")
	}
	if parsed.Scheme == "http" && !isLoopback(parsed.Hostname()) {
		return nil, fmt.Errorf("the API address must use HTTPS; plain HTTP is allowed only to localhost, not %s", parsed.Hostname())
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(parsed.Path, apiPrefix) {
		parsed.Path += apiPrefix
	}
	return parsed, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
}

// RootCAs is the system pool plus the certificates in caFile, if one is given.
func RootCAs(caFile string) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if caFile == "" {
		return pool, nil
	}
	content, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading CA file: %w", err)
	}
	if !pool.AppendCertsFromPEM(content) {
		return nil, fmt.Errorf("%s holds no PEM certificate", caFile)
	}
	return pool, nil
}

type Client struct {
	base      *url.URL
	http      *http.Client
	userAgent string
}

type Options struct {
	CAFile    string
	UserAgent string
	// RootCAs replaces the system pool; for tests.
	RootCAs *x509.CertPool
}

func NewClient(baseURL string, options Options) (*Client, error) {
	base, err := ValidateBaseURL(baseURL)
	if err != nil {
		return nil, err
	}

	roots := options.RootCAs
	if roots == nil {
		roots, err = RootCAs(options.CAFile)
		if err != nil {
			return nil, err
		}
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
	}

	return &Client{
		base: base,
		http: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		userAgent: options.UserAgent,
	}, nil
}

// Details describe the agent in registrations and heartbeats.
type Details struct {
	Name     string `json:"name,omitempty"`
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	// Recovery is sent with heartbeats only, and only while recovery is on locally.
	Recovery *RecoveryCapability `json:"recovery,omitempty"`
}

// RecoveryCapability names the protocol and the enabled actions, never their targets.
type RecoveryCapability struct {
	Protocol int      `json:"protocol"`
	Actions  []string `json:"actions"`
}

type Ref struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type IssuedCredential struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Scopes    []string  `json:"scopes"`
}

type Registration struct {
	Agent             Ref              `json:"agent"`
	Project           Ref              `json:"project"`
	Environment       Ref              `json:"environment"`
	Credential        IssuedCredential `json:"credential"`
	HeartbeatInterval int              `json:"heartbeat_interval"`
	MinimumVersion    string           `json:"minimum_version"`
}

type HeartbeatAnswer struct {
	HeartbeatInterval int `json:"heartbeat_interval"`
	Credential        struct {
		ExpiresAt time.Time `json:"expires_at"`
		Rotate    bool      `json:"rotate"`
	} `json:"credential"`
	MinimumVersion string `json:"minimum_version"`

	// ServerTime is the answer's Date header, zero when absent.
	ServerTime time.Time `json:"-"`
}

type rotation struct {
	Credential IssuedCredential `json:"credential"`
}

type Accepted struct {
	Accepted   int `json:"accepted"`
	Duplicates int `json:"duplicates"`

	ServerTime time.Time `json:"-"`
}

// TaskEnvelope is one signed recovery task: the payload bytes and their signature, both base64.
type TaskEnvelope struct {
	KeyID     string `json:"key_id"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type Tasks struct {
	Paused bool           `json:"paused"`
	Tasks  []TaskEnvelope `json:"tasks"`
}

const (
	StageReceived = "received"
	StageRunning  = "running"
	StageFinished = "finished"
	StageFailed   = "failed"
	StageRefused  = "refused"
)

type TaskReport struct {
	Nonce      string        `json:"nonce"`
	Stage      string        `json:"stage"`
	ReasonCode string        `json:"reason_code,omitempty"`
	Evidence   *TaskEvidence `json:"evidence,omitempty"`
}

// TaskEvidence is everything a report may say about a run. It never holds an answer body, a
// hook address or a token.
type TaskEvidence struct {
	DurationMs  *int64 `json:"duration_ms,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	HTTPStatus  int    `json:"http_status,omitempty"`
	CheckStatus string `json:"check_status,omitempty"`
}

type taskStatus struct {
	Status string `json:"status"`
}

func (client *Client) Register(ctx context.Context, enrollmentToken string, details Details) (*Registration, error) {
	details.Recovery = nil
	body := struct {
		EnrollmentToken string `json:"enrollment_token"`
		Details
	}{enrollmentToken, details}

	var registration Registration
	if _, err := client.do(ctx, http.MethodPost, pathRegister, "", body, &registration); err != nil {
		return nil, err
	}
	return &registration, nil
}

func (client *Client) Heartbeat(ctx context.Context, token string, details Details) (*HeartbeatAnswer, error) {
	details.Name = ""
	var answer HeartbeatAnswer
	header, err := client.do(ctx, http.MethodPost, pathHeartbeat, token, details, &answer)
	if err != nil {
		return nil, err
	}
	answer.ServerTime = serverTime(header)
	return &answer, nil
}

func (client *Client) Rotate(ctx context.Context, token string) (*IssuedCredential, error) {
	var answer rotation
	if _, err := client.do(ctx, http.MethodPost, pathRotate, token, struct{}{}, &answer); err != nil {
		return nil, err
	}
	if answer.Credential.Token == "" {
		return nil, &Error{Kind: Transient, Method: http.MethodPost, Path: pathRotate, Status: http.StatusCreated, Code: "missing_credential"}
	}
	return &answer.Credential, nil
}

// SendEvents sends one batch of already-encoded events.
func (client *Client) SendEvents(ctx context.Context, token string, events []json.RawMessage) (*Accepted, error) {
	body := struct {
		Events []json.RawMessage `json:"events"`
	}{events}

	var accepted Accepted
	header, err := client.do(ctx, http.MethodPost, pathTelemetry, token, body, &accepted)
	if err != nil {
		return nil, err
	}
	accepted.ServerTime = serverTime(header)
	return &accepted, nil
}

// GetTasks fetches the recovery tasks waiting for this agent: none or one.
func (client *Client) GetTasks(ctx context.Context, token string) (*Tasks, error) {
	var tasks Tasks
	if _, err := client.do(ctx, http.MethodGet, pathTasks, token, nil, &tasks); err != nil {
		return nil, err
	}
	return &tasks, nil
}

// ReportTask reports one stage of a task and returns the status the platform now holds for it.
// A refused transition (Conflict) returns that status together with the error.
func (client *Client) ReportTask(ctx context.Context, token, taskID string, report TaskReport) (string, error) {
	var answer taskStatus
	path := pathTasks + "/" + url.PathEscape(taskID) + "/result"
	_, err := client.do(ctx, http.MethodPost, path, token, report, &answer)
	return answer.Status, err
}

type envelope struct {
	IdenticalCode string          `json:"identical_code"`
	Message       string          `json:"message"`
	Content       json.RawMessage `json:"content"`
	Errors        json.RawMessage `json:"errors"`
}

// do sends one request. A nil body sends none, as GET requests need.
func (client *Client) do(ctx context.Context, method, path, token string, body any, content any) (http.Header, error) {
	var sent io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		sent = bytes.NewReader(encoded)
	}

	endpoint := client.base.JoinPath(path)
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), sent)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("User-Agent", client.userAgent)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := client.http.Do(request)
	if err != nil {
		return nil, &Error{Kind: Transient, Method: method, Path: path, Err: unwrapURLError(err)}
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(response.Body, maxAnswerBytes))
	if err != nil {
		return nil, &Error{Kind: Transient, Method: method, Path: path, Status: response.StatusCode, Err: unwrapURLError(err)}
	}

	var answer envelope
	decodeErr := json.Unmarshal(raw, &answer)

	if response.StatusCode < 200 || response.StatusCode > 299 {
		if response.StatusCode == http.StatusConflict && content != nil && len(answer.Content) > 0 {
			_ = json.Unmarshal(answer.Content, content)
		}
		return nil, classify(method, path, response, answer)
	}
	if decodeErr != nil {
		return nil, &Error{Kind: Transient, Method: method, Path: path, Status: response.StatusCode, Code: "invalid_answer", Err: decodeErr}
	}
	if content != nil && len(answer.Content) > 0 && string(answer.Content) != "null" {
		if err := json.Unmarshal(answer.Content, content); err != nil {
			return nil, &Error{Kind: Transient, Method: method, Path: path, Status: response.StatusCode, Code: "invalid_answer", Err: err}
		}
	}
	return response.Header, nil
}

func classify(method, path string, response *http.Response, answer envelope) *Error {
	apiError := &Error{
		Method:     method,
		Path:       path,
		Status:     response.StatusCode,
		Code:       answer.IdenticalCode,
		Message:    answer.Message,
		RetryAfter: RetryAfter(response.Header.Get("Retry-After"), time.Now()),
	}
	if len(answer.Errors) > 0 && answer.Errors[0] == '{' {
		_ = json.Unmarshal(answer.Errors, &apiError.Fields)
	}

	switch {
	case response.StatusCode == http.StatusUnauthorized && answer.IdenticalCode == "enrollment_token_invalid_401":
		apiError.Kind = TokenRefused
	case response.StatusCode == http.StatusUnauthorized:
		apiError.Kind = Unauthenticated
	case response.StatusCode == http.StatusForbidden && answer.IdenticalCode == "agent_scope_missing_403":
		apiError.Kind = Scope
	case response.StatusCode == http.StatusForbidden:
		apiError.Kind = Unavailable
	case response.StatusCode == http.StatusRequestEntityTooLarge, response.StatusCode == http.StatusUnprocessableEntity:
		apiError.Kind = Rejected
	case response.StatusCode == http.StatusTooManyRequests:
		apiError.Kind = RateLimited
	case response.StatusCode == http.StatusNotFound:
		apiError.Kind = NotFound
	case response.StatusCode == http.StatusConflict:
		apiError.Kind = Conflict
	default:
		apiError.Kind = Transient
	}
	return apiError
}

// unwrapURLError drops the request URL from transport errors, keeping the cause.
func unwrapURLError(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return urlError.Err
	}
	return err
}

func serverTime(header http.Header) time.Time {
	parsed, err := http.ParseTime(header.Get("Date"))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// RetryAfter reads a Retry-After header in seconds or as an HTTP date.
func RetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}
