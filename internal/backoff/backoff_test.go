package backoff

import (
	"testing"
	"time"
)

func TestWaitsGrowWithinBounds(t *testing.T) {
	policy := New(time.Second, 5*time.Minute)
	for attempt := 0; attempt < 40; attempt++ {
		step := time.Second << attempt
		if attempt >= 9 || step > 5*time.Minute {
			step = 5 * time.Minute
		}
		wait := policy.Next(0)
		if wait < step/2 || wait > step {
			t.Fatalf("attempt %d: wait %v outside [%v, %v]", attempt, wait, step/2, step)
		}
	}
}

func TestRetryAfterIsAFloor(t *testing.T) {
	policy := New(time.Second, 5*time.Minute)
	if wait := policy.Next(30 * time.Second); wait != 30*time.Second {
		t.Errorf("want the 30s floor, got %v", wait)
	}
	if wait := policy.Next(10 * time.Minute); wait != 10*time.Minute {
		t.Errorf("a Retry-After above the cap is still honored, got %v", wait)
	}
}

func TestReset(t *testing.T) {
	policy := New(time.Second, 5*time.Minute)
	for range 10 {
		policy.Next(0)
	}
	if policy.Failures() != 10 {
		t.Errorf("failures %d", policy.Failures())
	}
	policy.Reset()
	if wait := policy.Next(0); wait > time.Second {
		t.Errorf("after a reset the wait starts over, got %v", wait)
	}
}

func TestJitterSpreadsWaits(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 20 {
		policy := New(time.Second, 5*time.Minute)
		policy.Next(0)
		policy.Next(0)
		seen[policy.Next(0)] = true
	}
	if len(seen) < 10 {
		t.Errorf("waits should be randomized, saw %d distinct values", len(seen))
	}
}
