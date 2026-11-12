package check

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alimrun/autoheal-agent/internal/event"
)

func TestReporterConfirmsOverTwoRuns(t *testing.T) {
	reporter := NewReporter(2, 5*time.Minute)
	start := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

	if reporter.Observe(Result{Status: Up}, start) {
		t.Fatal("one run is not confirmed")
	}
	if !reporter.Observe(Result{Status: Up}, start.Add(30*time.Second)) {
		t.Fatal("the first confirmed status is reported at start")
	}
	if reporter.Observe(Result{Status: Down}, start.Add(60*time.Second)) {
		t.Fatal("a single down is not confirmed")
	}
	if !reporter.Observe(Result{Status: Down}, start.Add(90*time.Second)) {
		t.Fatal("two downs in a row are reported")
	}
	if reporter.Reported() != Down {
		t.Errorf("reported %s", reporter.Reported())
	}
}

func TestReporterIgnoresABlip(t *testing.T) {
	reporter := NewReporter(2, 5*time.Minute)
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	reporter.Observe(Result{Status: Up}, now)
	reporter.Observe(Result{Status: Up}, now.Add(time.Minute))

	if reporter.Observe(Result{Status: Down}, now.Add(2*time.Minute)) || reporter.Observe(Result{Status: Up}, now.Add(3*time.Minute)) {
		t.Fatal("a blip that recovers on the next run reports nothing")
	}
	if reporter.Reported() != Up {
		t.Errorf("reported %s", reporter.Reported())
	}
}

func TestReporterRefreshesEveryFiveMinutes(t *testing.T) {
	reporter := NewReporter(2, 5*time.Minute)
	start := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	reports := 0
	for second := 0; second < 3600; second += 30 {
		if reporter.Observe(Result{Status: Up}, start.Add(time.Duration(second)*time.Second)) {
			reports++
		}
	}
	if reports != 12 {
		t.Errorf("a steady hour reports %d times, want 12", reports)
	}
}

func TestReporterSkipsEmptyResults(t *testing.T) {
	reporter := NewReporter(2, 5*time.Minute)
	now := time.Now()
	if reporter.Observe(Result{Skip: true}, now) || reporter.Observe(Result{Skip: true}, now) {
		t.Fatal("skipped results report nothing")
	}
}

func TestCPUFirstReportsOnItsThirdTick(t *testing.T) {
	samples := []CPUTimes{{Busy: 0, Total: 100}, {Busy: 96, Total: 200}, {Busy: 192, Total: 300}}
	probe := &CPUProbe{Degraded: 85, Down: 95, Read: func(context.Context) (CPUTimes, error) {
		sample := samples[0]
		samples = samples[1:]
		return sample, nil
	}}
	reporter := NewReporter(2, 5*time.Minute)
	now := time.Now()

	results := []Result{probe.Run(context.Background()), probe.Run(context.Background()), probe.Run(context.Background())}
	if !results[0].Skip {
		t.Fatal("the first run only samples")
	}
	if results[1].Status != Down || results[1].ValuePercent != 96 || results[1].Detail != "96% CPU used" {
		t.Fatalf("second run: %+v", results[1])
	}

	reported := []bool{}
	for _, result := range results {
		reported = append(reported, reporter.Observe(result, now))
	}
	if reported[0] || reported[1] || !reported[2] {
		t.Errorf("reports %v, want only the third", reported)
	}
}

func TestPercentProbe(t *testing.T) {
	cases := map[float64]Status{50: Up, 85: Degraded, 94.9: Degraded, 95: Down, 100: Down}
	for percent, want := range cases {
		probe := &PercentProbe{Degraded: 85, Down: 95, What: "of /", Read: func(context.Context) (float64, error) { return percent, nil }}
		if got := probe.Run(context.Background()); got.Status != want {
			t.Errorf("%g%%: %s, want %s", percent, got.Status, want)
		}
	}

	failing := &PercentProbe{Degraded: 85, Down: 95, What: "of /data", Read: func(context.Context) (float64, error) {
		return 0, errors.New("no such file or directory")
	}}
	result := failing.Run(context.Background())
	if result.Status != Down || !strings.HasPrefix(result.Detail, "cannot read of /data") {
		t.Errorf("unreadable disk: %+v", result)
	}
}

func TestHostReadersWorkOnThisMachine(t *testing.T) {
	if _, err := ReadCPUTimes(context.Background()); err != nil {
		t.Errorf("cpu: %v", err)
	}
	if percent, err := ReadMemoryPercent(context.Background()); err != nil || percent <= 0 || percent > 100 {
		t.Errorf("memory: %v %v", percent, err)
	}
	if percent, err := DiskPercentReader("/")(context.Background()); err != nil || percent <= 0 || percent > 100 {
		t.Errorf("disk: %v %v", percent, err)
	}
}

func httpProbe(url string, slow time.Duration) *HTTPProbe {
	return &HTTPProbe{URL: url, Slow: slow, Client: NewHTTPClient(nil, "autoheal-agent/test check")}
}

func TestHTTPProbe(t *testing.T) {
	var userAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent = r.Header.Get("User-Agent")
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(200)
		case "/error":
			w.WriteHeader(500)
		case "/slow":
			time.Sleep(60 * time.Millisecond)
			w.WriteHeader(200)
		case "/redirect":
			http.Redirect(w, r, "/error", http.StatusFound)
		case "/large":
			_, _ = w.Write([]byte(strings.Repeat("a", 10<<20)))
		}
	}))
	defer server.Close()

	cases := map[string]struct {
		slow   time.Duration
		status Status
		detail string
	}{
		"/ok":       {0, Up, "status 200 in"},
		"/error":    {0, Down, "status 500"},
		"/slow":     {20 * time.Millisecond, Degraded, "slower than 20 ms"},
		"/redirect": {0, Up, "status 302"},
		"/large":    {0, Up, "status 200"},
	}
	for path, want := range cases {
		result := httpProbe(server.URL+path, want.slow).Run(context.Background())
		if result.Status != want.status || !strings.Contains(result.Detail, want.detail) || !result.HasLatency {
			t.Errorf("%s: %+v", path, result)
		}
	}
	if userAgent != "autoheal-agent/test check" {
		t.Errorf("user agent %q", userAgent)
	}
}

func TestHTTPExpectStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	probe := httpProbe(server.URL, 0)
	probe.ExpectStatus = []int{200}
	if result := probe.Run(context.Background()); result.Status != Down || result.Detail != "status 204" {
		t.Errorf("unexpected status: %+v", result)
	}
}

func closedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

func TestFailingURLNeverLeaksItsQuery(t *testing.T) {
	address := closedAddress(t)
	rawURL := "http://" + address + "/health?token=abc"
	check := &Check{
		Name:         "web-01:api",
		Kind:         KindHTTP,
		Target:       event.SanitizeURL(rawURL),
		Timeout:      time.Second,
		DownSeverity: "error",
		Probe:        httpProbe(rawURL, 0),
	}

	result := check.Run(context.Background())
	if result.Status != Down || result.Detail != "connection refused" {
		t.Fatalf("result: %+v", result)
	}
	built := check.Event(result, time.Now(), "0.1.0")
	encoded := built.Message + " " + built.Context["target"].(string)
	if strings.Contains(encoded, "token") || strings.Contains(encoded, "abc") {
		t.Errorf("the query leaked: %s", encoded)
	}
	if built.Context["target"] != "http://"+address+"/health" {
		t.Errorf("target %v", built.Context["target"])
	}
}

func TestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	check := &Check{Name: "web-01:api", Timeout: 50 * time.Millisecond, Probe: httpProbe(server.URL, 0)}
	if result := check.Run(context.Background()); result.Status != Down || result.Detail != "timed out" {
		t.Errorf("timeout: %+v", result)
	}
}

func TestTCPProbe(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connection.Close()
		}
	}()

	if result := (&TCPProbe{Address: listener.Addr().String()}).Run(context.Background()); result.Status != Up || !strings.HasPrefix(result.Detail, "connected in") {
		t.Errorf("open port: %+v", result)
	}
	if result := (&TCPProbe{Address: closedAddress(t)}).Run(context.Background()); result.Status != Down || result.Detail != "connection refused" {
		t.Errorf("closed port: %+v", result)
	}

	ticks := []time.Time{time.Unix(0, 0), time.Unix(0, int64(450*time.Millisecond))}
	slow := &TCPProbe{Address: listener.Addr().String(), Slow: 200 * time.Millisecond, now: func() time.Time {
		tick := ticks[0]
		ticks = ticks[1:]
		return tick
	}}
	result := slow.Run(context.Background())
	if result.Status != Degraded || result.Latency != 450*time.Millisecond {
		t.Errorf("slow: %+v", result)
	}
}

func TestEventSeverityAndBody(t *testing.T) {
	check := &Check{Name: "web-01:api", Kind: KindTCP, Target: "127.0.0.1:5432", DownSeverity: "critical"}
	cases := map[Status]string{Up: "info", Degraded: "warning", Down: "critical"}
	for status, want := range cases {
		built := check.Event(Result{Status: status, Latency: 12 * time.Millisecond, HasLatency: true, Detail: "x"}, time.Now(), "0.1.0")
		if built.Severity != want || built.Body.Status != string(status) || *built.Body.LatencyMs != 12 || built.Type != "health_check" {
			t.Errorf("%s: %+v", status, built)
		}
	}

	check.DownSeverity = "error"
	built := check.Event(Result{Status: Down, Latency: time.Hour, HasLatency: true}, time.Now(), "0.1.0")
	if built.Severity != "error" || *built.Body.LatencyMs != 600000 || built.Message != "web-01:api is down" {
		t.Errorf("default severity and latency cap: %+v", built)
	}

	host := &Check{Name: "web-01:host.cpu", Kind: KindCPU, DownSeverity: "error"}
	built = host.Event(Result{Status: Degraded, ValuePercent: 91.26, HasValue: true, Detail: "91% CPU used"}, time.Now(), "0.1.0")
	if built.Body.LatencyMs != nil || built.Context["value_percent"] != 91.3 || built.Message != "web-01:host.cpu is degraded: 91% CPU used" {
		t.Errorf("host event: %+v", built)
	}
}
