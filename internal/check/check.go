// Package check runs the agent's health checks and decides which results to report.
package check

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/alimrun/autoheal-agent/internal/event"
)

type Status string

const (
	Up       Status = "up"
	Degraded Status = "degraded"
	Down     Status = "down"
)

// Result is one run of a probe. Skip means the probe has nothing to report yet.
type Result struct {
	Status       Status
	Latency      time.Duration
	HasLatency   bool
	ValuePercent float64
	HasValue     bool
	Detail       string
	Skip         bool
}

type Probe interface {
	Run(ctx context.Context) Result
}

type Kind string

const (
	KindCPU    Kind = "cpu"
	KindMemory Kind = "memory"
	KindDisk   Kind = "disk"
	KindHTTP   Kind = "http"
	KindTCP    Kind = "tcp"
)

// Check is one configured check, named as it is reported: "<prefix>:<name>".
type Check struct {
	Name         string
	Kind         Kind
	Target       string
	Interval     time.Duration
	Timeout      time.Duration
	DownSeverity string
	Probe        Probe
}

func (check *Check) Run(ctx context.Context) Result {
	runContext, cancel := context.WithTimeout(ctx, check.Timeout)
	defer cancel()
	return check.Probe.Run(runContext)
}

func severity(status Status, downSeverity string) string {
	switch status {
	case Up:
		return "info"
	case Degraded:
		return "warning"
	default:
		return downSeverity
	}
}

const maxLatencyMs = 600000

// Event builds the health_check event reporting result for check.
func (check *Check) Event(result Result, at time.Time, agentVersion string) event.Event {
	message := fmt.Sprintf("%s is %s", check.Name, result.Status)
	if result.Detail != "" {
		message += ": " + result.Detail
	}
	if len(message) > 300 {
		message = message[:297] + "..."
	}

	context := map[string]any{"agent_version": agentVersion, "kind": string(check.Kind)}
	if check.Target != "" {
		context["target"] = check.Target
	}
	if result.HasValue {
		context["value_percent"] = math.Round(result.ValuePercent*10) / 10
	}

	body := event.HealthCheck{Name: check.Name, Status: string(result.Status)}
	if result.HasLatency {
		latency := min(result.Latency.Milliseconds(), maxLatencyMs)
		body.LatencyMs = &latency
	}

	return event.Event{
		SchemaVersion: event.SchemaVersion,
		EventID:       event.NewID(at),
		Type:          "health_check",
		Severity:      severity(result.Status, check.DownSeverity),
		OccurredAt:    event.Timestamp{Time: at},
		Message:       message,
		Context:       context,
		Body:          body,
	}
}

// Reporter decides when a check's results are reported: once a new status has held for
// Confirm runs in a row, and otherwise every Refresh while the status stays the same.
type Reporter struct {
	Confirm int
	Refresh time.Duration

	reported  Status
	candidate Status
	streak    int
	lastSent  time.Time
}

func NewReporter(confirm int, refresh time.Duration) *Reporter {
	return &Reporter{Confirm: confirm, Refresh: refresh}
}

// Observe records one result and reports whether it should be sent.
func (reporter *Reporter) Observe(result Result, now time.Time) bool {
	if result.Skip {
		return false
	}

	if result.Status != reporter.candidate {
		reporter.candidate = result.Status
		reporter.streak = 1
	} else {
		reporter.streak++
	}

	if reporter.streak >= reporter.Confirm && reporter.candidate != reporter.reported {
		reporter.reported = reporter.candidate
		reporter.lastSent = now
		return true
	}

	if reporter.reported != "" && result.Status == reporter.reported && now.Sub(reporter.lastSent) >= reporter.Refresh {
		reporter.lastSent = now
		return true
	}

	return false
}

// Reported is the last status sent, empty before the first report.
func (reporter *Reporter) Reported() Status {
	return reporter.reported
}

// statusOf compares a percentage with degraded and down thresholds.
func statusOf(percent, degraded, down float64) Status {
	switch {
	case percent >= down:
		return Down
	case percent >= degraded:
		return Degraded
	default:
		return Up
	}
}
