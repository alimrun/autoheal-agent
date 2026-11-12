package check

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alimrun/autoheal-agent/internal/event"
)

var update = flag.Bool("update", false, "rewrite testdata/events.golden.json")

const goldenPath = "../../testdata/events.golden.json"

// fixedClock makes a probe measure exactly latency between its two clock reads.
func fixedClock(latency time.Duration) func() time.Time {
	start := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	reads := 0
	return func() time.Time {
		reads++
		if reads%2 == 0 {
			return start.Add(latency)
		}
		return start
	}
}

type goldenCase struct {
	check  *Check
	result Result
}

func hostCases(t *testing.T) []goldenCase {
	t.Helper()
	var cases []goldenCase

	for _, percent := range []float64{42.36, 88.04, 97.51} {
		samples := []CPUTimes{{Busy: 0, Total: 100}, {Busy: percent, Total: 200}}
		probe := &CPUProbe{Degraded: 85, Down: 95, Read: func(context.Context) (CPUTimes, error) {
			sample := samples[0]
			samples = samples[1:]
			return sample, nil
		}}
		probe.Run(context.Background())
		cases = append(cases, goldenCase{&Check{Name: "web-01:host.cpu", Kind: KindCPU, DownSeverity: "error"}, probe.Run(context.Background())})
	}

	for _, host := range []struct {
		name, what string
		kind       Kind
	}{
		{"web-01:host.memory", "memory", KindMemory},
		{"web-01:host.disk.root", "of /", KindDisk},
	} {
		for _, percent := range []float64{61.27, 90.55, 98.04} {
			probe := &PercentProbe{Degraded: 85, Down: 95, What: host.what, Read: func(context.Context) (float64, error) { return percent, nil }}
			cases = append(cases, goldenCase{&Check{Name: host.name, Kind: host.kind, DownSeverity: "error"}, probe.Run(context.Background())})
		}
	}
	return cases
}

func goldenEvents(t *testing.T) []event.Event {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/broken" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

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
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddress := closed.Addr().String()
	closed.Close()

	client := NewHTTPClient(nil, "autoheal-agent/0.1.0 check")
	httpRun := func(path string, latency time.Duration) Result {
		probe := &HTTPProbe{URL: server.URL + path, Slow: time.Second, Client: client, now: fixedClock(latency)}
		return probe.Run(context.Background())
	}
	tcpRun := func(address string, latency time.Duration) Result {
		probe := &TCPProbe{Address: address, Slow: 500 * time.Millisecond, now: fixedClock(latency)}
		return probe.Run(context.Background())
	}

	api := &Check{Name: "web-01:api", Kind: KindHTTP, Target: event.SanitizeURL("https://127.0.0.1:8443/health?token=not-sent#status"), DownSeverity: "error"}
	postgres := &Check{Name: "web-01:postgres", Kind: KindTCP, Target: "127.0.0.1:5432", DownSeverity: "error"}
	payments := &Check{Name: "web-01:payments", Kind: KindHTTP, Target: event.SanitizeURL("https://127.0.0.1:9443/ready"), DownSeverity: "critical"}

	cases := append(hostCases(t), []goldenCase{
		{api, httpRun("/broken", 12*time.Millisecond)},
		{api, httpRun("/health", 1450*time.Millisecond)},
		{api, httpRun("/health", 38*time.Millisecond)},
		{postgres, tcpRun(closedAddress, 2*time.Millisecond)},
		{postgres, tcpRun(listener.Addr().String(), 740*time.Millisecond)},
		{postgres, tcpRun(listener.Addr().String(), 3*time.Millisecond)},
		{payments, httpRun("/broken", 25*time.Millisecond)},
	}...)

	occurredAt := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	events := make([]event.Event, 0, len(cases))
	for index, item := range cases {
		built := item.check.Event(item.result, occurredAt.Add(time.Duration(index)*time.Second), "0.1.0")
		built.EventID = fmt.Sprintf("01JCGG0000000000000000%04d", index+1)
		events = append(events, built)
	}
	return events
}

func TestGoldenEvents(t *testing.T) {
	encoded, err := json.MarshalIndent(goldenEvents(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')

	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading %s (regenerate with go test ./internal/check -run Golden -update): %v", goldenPath, err)
	}
	if !bytes.Equal(golden, encoded) {
		t.Errorf("%s is out of date; regenerate with go test ./internal/check -run Golden -update\n\ngot:\n%s", goldenPath, encoded)
	}
}
