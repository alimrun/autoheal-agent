// Package event builds telemetry events in schema version 1 (contracts/telemetry-event.schema.json).
package event

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/url"
	"time"
)

const SchemaVersion = 1

const timestampLayout = "2006-01-02T15:04:05.000Z07:00"

// Timestamp encodes as RFC 3339 in UTC with milliseconds; the platform allows at most six
// fractional digits, and Go's default encoding can write nine.
type Timestamp struct{ time.Time }

func (timestamp Timestamp) MarshalJSON() ([]byte, error) {
	return []byte(`"` + timestamp.UTC().Format(timestampLayout) + `"`), nil
}

func (timestamp *Timestamp) UnmarshalJSON(data []byte) error {
	parsed, err := time.Parse(`"`+time.RFC3339Nano+`"`, string(data))
	if err != nil {
		return err
	}
	timestamp.Time = parsed
	return nil
}

type Event struct {
	SchemaVersion int            `json:"schema_version"`
	EventID       string         `json:"event_id"`
	Type          string         `json:"type"`
	Severity      string         `json:"severity"`
	OccurredAt    Timestamp      `json:"occurred_at"`
	Message       string         `json:"message"`
	Context       map[string]any `json:"context,omitempty"`
	Body          HealthCheck    `json:"body"`
}

type HealthCheck struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	LatencyMs *int64 `json:"latency_ms,omitempty"`
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID returns a ULID: 48 bits of milliseconds then 80 random bits, as 26 Crockford characters.
func NewID(at time.Time) string {
	var raw [16]byte
	milliseconds := uint64(at.UnixMilli())
	raw[0] = byte(milliseconds >> 40)
	raw[1] = byte(milliseconds >> 32)
	binary.BigEndian.PutUint32(raw[2:6], uint32(milliseconds))
	if _, err := rand.Read(raw[6:]); err != nil {
		panic(fmt.Sprintf("reading random bytes: %v", err))
	}

	high := binary.BigEndian.Uint64(raw[0:8])
	low := binary.BigEndian.Uint64(raw[8:16])
	var encoded [26]byte
	for index := 25; index >= 0; index-- {
		encoded[index] = crockford[low&31]
		low = low>>5 | high<<59
		high >>= 5
	}
	return string(encoded[:])
}

// SanitizeURL keeps a URL's scheme, host, port and path, dropping user information, query and fragment.
func SanitizeURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "(invalid URL)"
	}
	clean := url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: parsed.Path}
	return clean.String()
}
