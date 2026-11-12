// Package backoff computes randomized, exponentially growing waits between retries.
package backoff

import (
	"math/rand/v2"
	"time"
)

type Policy struct {
	Base    time.Duration
	Max     time.Duration
	attempt int
	random  func() float64
}

func New(base, max time.Duration) *Policy {
	return &Policy{Base: base, Max: max, random: rand.Float64}
}

// Next returns the wait before the next retry: a random point in the upper half of the
// current exponential step, never below floor (a Retry-After), and never above Max unless
// floor asks for more.
func (policy *Policy) Next(floor time.Duration) time.Duration {
	step := policy.Max
	if policy.attempt < 32 {
		if grown := policy.Base << policy.attempt; grown > 0 && grown < policy.Max {
			step = grown
		}
	}
	policy.attempt++

	wait := step/2 + time.Duration(policy.random()*float64(step/2))
	if wait < floor {
		return floor
	}
	return wait
}

func (policy *Policy) Reset() {
	policy.attempt = 0
}

// Failures is how many waits were handed out since the last reset.
func (policy *Policy) Failures() int {
	return policy.attempt
}
