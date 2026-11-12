package api

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func answer(w http.ResponseWriter, status int, code string, content any, errors any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if errors == nil {
		errors = []any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"identical_code": code, "message": "m", "content": content, "errors": errors})
}

func client(t *testing.T, url string) *Client {
	t.Helper()
	created, err := NewClient(url, Options{UserAgent: "autoheal-agent/test (os/arch)"})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func TestValidateBaseURL(t *testing.T) {
	accepted := map[string]string{
		"https://autoheal.example.com":         "https://autoheal.example.com/api/v1",
		"https://autoheal.example.com/":        "https://autoheal.example.com/api/v1",
		"https://autoheal.example.com/api/v1/": "https://autoheal.example.com/api/v1",
		"http://localhost:8100/api/v1":         "http://localhost:8100/api/v1",
		"http://127.0.0.1:8100":                "http://127.0.0.1:8100/api/v1",
		"http://[::1]:8100":                    "http://[::1]:8100/api/v1",
	}
	for raw, want := range accepted {
		parsed, err := ValidateBaseURL(raw)
		if err != nil || parsed.String() != want {
			t.Errorf("%s: got %v %v, want %s", raw, parsed, err, want)
		}
	}

	refused := map[string]string{
		"http://autoheal.example.com":      "must use HTTPS",
		"http://localhost.example.com":     "must use HTTPS",
		"ftp://autoheal.example.com":       "not an http or https address",
		"https://user:pass@autoheal.test":  "must not contain",
		"https://autoheal.test/?token=abc": "must not contain",
		"autoheal.example.com":             "not an http or https address",
	}
	for raw, want := range refused {
		_, err := ValidateBaseURL(raw)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", raw, want, err)
		}
	}
}

func TestHeartbeatSendsCredentialAndDetails(t *testing.T) {
	var seen *http.Request
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		_ = json.NewDecoder(r.Body).Decode(&body)
		answer(w, 200, "default_200", map[string]any{
			"heartbeat_interval": 60,
			"credential":         map[string]any{"expires_at": "2027-01-01T00:00:00+00:00", "rotate": true},
			"minimum_version":    "0.2.0",
			"tasks":              []string{"ignored"},
		}, nil)
	}))
	defer server.Close()

	result, err := client(t, server.URL).Heartbeat(context.Background(), "secret-token", Details{Name: "ignored", Hostname: "web-01", Version: "0.1.0", OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if seen.URL.Path != "/api/v1/agent/heartbeat" || seen.Header.Get("Authorization") != "Bearer secret-token" {
		t.Errorf("request: %s %v", seen.URL.Path, seen.Header)
	}
	if seen.Header.Get("User-Agent") != "autoheal-agent/test (os/arch)" || seen.URL.RawQuery != "" {
		t.Errorf("user agent or query: %v %q", seen.Header.Get("User-Agent"), seen.URL.RawQuery)
	}
	if _, hasToken := body["token"]; hasToken || body["hostname"] != "web-01" || body["name"] != nil {
		t.Errorf("body: %v", body)
	}
	if result.HeartbeatInterval != 60 || !result.Credential.Rotate || result.MinimumVersion != "0.2.0" || result.ServerTime.IsZero() {
		t.Errorf("answer: %+v", result)
	}
}

func TestAnswersAreClassified(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   Kind
	}{
		{401, "enrollment_token_invalid_401", TokenRefused},
		{401, "agent_unauthenticated_401", Unauthenticated},
		{403, "agent_scope_missing_403", Scope},
		{403, "agent_unavailable_403", Unavailable},
		{413, "payload_too_large_413", Rejected},
		{422, "validation_failed_422", Rejected},
		{429, "http_429", RateLimited},
		{500, "", Transient},
		{503, "", Transient},
	}

	for _, testCase := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			answer(w, testCase.status, testCase.code, nil, nil)
		}))
		_, err := client(t, server.URL).Heartbeat(context.Background(), "token", Details{})
		server.Close()
		if KindOf(err) != testCase.want {
			t.Errorf("%d %s: want %v, got %v (%v)", testCase.status, testCase.code, testCase.want, KindOf(err), err)
		}
		if err != nil && strings.Contains(err.Error(), "token") && testCase.code != "enrollment_token_invalid_401" {
			t.Errorf("error should not mention the token: %v", err)
		}
	}
}

func TestFieldErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer(w, 422, "validation_failed_422", nil, map[string][]string{"events.0.occurred_at": {"The field must be a date before or equal to now."}})
	}))
	defer server.Close()

	_, err := client(t, server.URL).SendEvents(context.Background(), "token", []json.RawMessage{json.RawMessage(`{}`)})
	var apiError *Error
	if KindOf(err) != Rejected || !asError(err, &apiError) || !strings.Contains(apiError.FieldSummary(), "events.0.occurred_at") {
		t.Fatalf("unexpected: %v", err)
	}
}

func asError(err error, target **Error) bool {
	apiError, ok := err.(*Error)
	*target = apiError
	return ok
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	if got := RetryAfter("30", now); got != 30*time.Second {
		t.Errorf("seconds: %v", got)
	}
	if got := RetryAfter(now.Add(90*time.Second).Format(http.TimeFormat), now); got != 90*time.Second {
		t.Errorf("date: %v", got)
	}
	if got := RetryAfter("soon", now); got != 0 {
		t.Errorf("garbage: %v", got)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		answer(w, 429, "http_429", nil, nil)
	}))
	defer server.Close()
	_, err := client(t, server.URL).SendEvents(context.Background(), "token", nil)
	if KindOf(err) != RateLimited || RetryAfterOf(err) != 30*time.Second {
		t.Errorf("unexpected: %v %v", err, RetryAfterOf(err))
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		answer(w, 200, "default_200", map[string]any{}, nil)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	_, err := client(t, origin.URL).Heartbeat(context.Background(), "secret-token", Details{})
	if err == nil {
		t.Fatal("a redirect must not count as success")
	}
	if targetHits.Load() != 0 {
		t.Error("the redirect target was contacted, so the credential could have leaked")
	}
}

func TestTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer(w, 202, "telemetry_accepted_202", map[string]int{"accepted": 2, "duplicates": 1}, nil)
	}))
	defer server.Close()

	untrusted := client(t, server.URL)
	if _, err := untrusted.SendEvents(context.Background(), "token", nil); KindOf(err) != Transient || err == nil {
		t.Fatalf("an unknown certificate authority must be refused: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	trusted, err := NewClient(server.URL, Options{RootCAs: pool})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := trusted.SendEvents(context.Background(), "token", []json.RawMessage{json.RawMessage(`{}`)})
	if err != nil || accepted.Accepted != 2 || accepted.Duplicates != 1 {
		t.Fatalf("trusted CA: %v %+v", err, accepted)
	}
}

func TestOversizedAnswerIsTruncated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"content":"` + strings.Repeat("a", 2<<20) + `"}`))
	}))
	defer server.Close()

	_, err := client(t, server.URL).Heartbeat(context.Background(), "token", Details{})
	if KindOf(err) != Transient || err == nil || !strings.Contains(err.Error(), "invalid_answer") {
		t.Errorf("unexpected: %v", err)
	}
}

func TestNetworkErrorHasNoURL(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	address := server.URL
	server.Close()

	_, err := client(t, address).Heartbeat(context.Background(), "token", Details{})
	if KindOf(err) != Transient || strings.Contains(err.Error(), address) {
		t.Errorf("unexpected: %v", err)
	}
}

func TestRegisterAndRotate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/register":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Header.Get("Authorization") != "" || body["enrollment_token"] != "enroll-me" || body["name"] != "web-01" {
				answer(w, 422, "validation_failed_422", nil, nil)
				return
			}
			answer(w, 201, "default_201", map[string]any{
				"agent":              map[string]string{"id": "A1", "name": "web-01"},
				"project":            map[string]string{"id": "P1", "name": "Shop"},
				"environment":        map[string]string{"id": "E1", "name": "production", "type": "production"},
				"credential":         map[string]any{"token": "issued", "expires_at": "2027-02-01T00:00:00+00:00", "scopes": []string{"heartbeat", "telemetry"}},
				"heartbeat_interval": 60,
				"minimum_version":    "0.1.0",
			}, nil)
		case "/api/v1/agent/credential/rotate":
			answer(w, 201, "default_201", map[string]any{"credential": map[string]any{"token": "rotated", "expires_at": "2027-05-01T00:00:00+00:00"}}, nil)
		}
	}))
	defer server.Close()

	api := client(t, server.URL)
	registration, err := api.Register(context.Background(), "enroll-me", Details{Name: "web-01", Hostname: "web-01", Version: "0.1.0", OS: "linux", Arch: "amd64"})
	if err != nil || registration.Credential.Token != "issued" || registration.Environment.Name != "production" || registration.MinimumVersion != "0.1.0" {
		t.Fatalf("register: %v %+v", err, registration)
	}
	rotated, err := api.Rotate(context.Background(), "issued")
	if err != nil || rotated.Token != "rotated" {
		t.Fatalf("rotate: %v %+v", err, rotated)
	}
}
