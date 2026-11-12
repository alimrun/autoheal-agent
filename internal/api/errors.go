package api

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type Kind int

const (
	// Transient covers network failures, timeouts, TLS failures and 5xx answers.
	Transient Kind = iota
	RateLimited
	// Unavailable means the environment, project or organization cannot be used right now.
	Unavailable
	// Rejected means the request itself is invalid or too large and must not be resent.
	Rejected
	// Unauthenticated means the credential is unknown, expired or revoked.
	Unauthenticated
	// Scope means the credential lacks the scope; the agent must be enrolled again.
	Scope
	// TokenRefused means an enrollment token was expired, cancelled or already used.
	TokenRefused
)

func (kind Kind) String() string {
	return [...]string{"transient", "rate_limited", "unavailable", "rejected", "unauthenticated", "scope", "token_refused"}[kind]
}

// Error never holds request headers or bodies, so it is safe to log.
type Error struct {
	Kind       Kind
	Path       string
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
	Fields     map[string][]string
	Err        error
}

func (apiError *Error) Error() string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "POST %s", apiError.Path)
	if apiError.Status != 0 {
		fmt.Fprintf(&builder, ": %d", apiError.Status)
	}
	if apiError.Code != "" {
		fmt.Fprintf(&builder, " %s", apiError.Code)
	}
	if apiError.Err != nil {
		fmt.Fprintf(&builder, ": %v", apiError.Err)
	}
	return builder.String()
}

func (apiError *Error) Unwrap() error { return apiError.Err }

// FieldSummary lists the platform's field errors in a stable order, for the log.
func (apiError *Error) FieldSummary() string {
	fields := make([]string, 0, len(apiError.Fields))
	for field := range apiError.Fields {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		parts = append(parts, field+": "+strings.Join(apiError.Fields[field], "; "))
	}
	return strings.Join(parts, " | ")
}

// KindOf is the kind of err, or Transient when err is not an API error.
func KindOf(err error) Kind {
	var apiError *Error
	if errors.As(err, &apiError) {
		return apiError.Kind
	}
	return Transient
}

func RetryAfterOf(err error) time.Duration {
	var apiError *Error
	if errors.As(err, &apiError) {
		return apiError.RetryAfter
	}
	return 0
}
