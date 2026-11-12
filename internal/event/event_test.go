package event

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
)

var (
	idPattern        = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	timestampPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,6})?(Z|[+-]\d{2}:\d{2})$`)
)

func TestIDsAreValidAndUnique(t *testing.T) {
	now := time.Now()
	seen := make(map[string]bool, 100000)
	for range 100000 {
		id := NewID(now)
		if len(id) != 26 || !idPattern.MatchString(id) {
			t.Fatalf("invalid id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestIDsSortByTime(t *testing.T) {
	earlier := NewID(time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC))
	later := NewID(time.Date(2026, 11, 11, 10, 0, 1, 0, time.UTC))
	if earlier >= later {
		t.Errorf("%s should sort before %s", earlier, later)
	}
}

func TestTimestampMatchesThePlatformPattern(t *testing.T) {
	at := time.Date(2026, 11, 11, 10, 0, 0, 123456789, time.FixedZone("x", 3600))
	encoded, err := json.Marshal(Timestamp{at})
	if err != nil {
		t.Fatal(err)
	}
	value := strings.Trim(string(encoded), `"`)
	if value != "2026-11-11T09:00:00.123Z" || !timestampPattern.MatchString(value) {
		t.Errorf("timestamp %s", value)
	}

	var decoded Timestamp
	if err := json.Unmarshal(encoded, &decoded); err != nil || !decoded.Equal(at.Truncate(time.Millisecond)) {
		t.Errorf("round trip: %v %v", err, decoded)
	}
}

func TestSanitizeURL(t *testing.T) {
	cases := map[string]string{
		"https://api.example.com/health?token=abc":        "https://api.example.com/health",
		"https://user:secret@api.example.com:8443/h#frag": "https://api.example.com:8443/h",
		"http://127.0.0.1:8080":                           "http://127.0.0.1:8080",
		"not a url":                                       "(invalid URL)",
	}
	for raw, want := range cases {
		if got := SanitizeURL(raw); got != want {
			t.Errorf("SanitizeURL(%q) = %q, want %q", raw, got, want)
		}
	}
}
