package check

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

const maxCheckAnswerBytes = 64 << 10

// NewHTTPClient is shared by HTTP checks: no redirects, no proxy, TLS verified against roots.
func NewHTTPClient(roots *x509.CertPool, userAgent string) *http.Client {
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		TLSHandshakeTimeout: 5 * time.Second,
		DisableKeepAlives:   true,
	}
	return &http.Client{
		Transport: userAgentTransport{base: transport, userAgent: userAgent},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type userAgentTransport struct {
	base      http.RoundTripper
	userAgent string
}

func (transport userAgentTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request.Header.Set("User-Agent", transport.userAgent)
	return transport.base.RoundTrip(request)
}

type HTTPProbe struct {
	URL          string
	ExpectStatus []int
	Slow         time.Duration
	Client       *http.Client
	now          func() time.Time
}

func (probe *HTTPProbe) clock() time.Time {
	if probe.now != nil {
		return probe.now()
	}
	return time.Now()
}

func (probe *HTTPProbe) expected(status int) bool {
	if len(probe.ExpectStatus) == 0 {
		return status >= 200 && status <= 399
	}
	return slices.Contains(probe.ExpectStatus, status)
}

func (probe *HTTPProbe) Run(ctx context.Context) Result {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, probe.URL, nil)
	if err != nil {
		return Result{Status: Down, Detail: "invalid URL"}
	}

	started := probe.clock()
	response, err := probe.Client.Do(request)
	latency := probe.clock().Sub(started)
	if err != nil {
		return Result{Status: Down, Latency: latency, HasLatency: true, Detail: describe(err)}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxCheckAnswerBytes))
	response.Body.Close()

	switch {
	case !probe.expected(response.StatusCode):
		return Result{Status: Down, Latency: latency, HasLatency: true, Detail: fmt.Sprintf("status %d", response.StatusCode)}
	case probe.Slow > 0 && latency > probe.Slow:
		return Result{Status: Degraded, Latency: latency, HasLatency: true, Detail: fmt.Sprintf("status %d in %d ms, slower than %d ms", response.StatusCode, latency.Milliseconds(), probe.Slow.Milliseconds())}
	default:
		return Result{Status: Up, Latency: latency, HasLatency: true, Detail: fmt.Sprintf("status %d in %d ms", response.StatusCode, latency.Milliseconds())}
	}
}

type TCPProbe struct {
	Address string
	Slow    time.Duration
	now     func() time.Time
}

func (probe *TCPProbe) Run(ctx context.Context) Result {
	clock := time.Now
	if probe.now != nil {
		clock = probe.now
	}

	started := clock()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", probe.Address)
	latency := clock().Sub(started)
	if err != nil {
		return Result{Status: Down, Latency: latency, HasLatency: true, Detail: describe(err)}
	}
	connection.Close()

	if probe.Slow > 0 && latency > probe.Slow {
		return Result{Status: Degraded, Latency: latency, HasLatency: true, Detail: fmt.Sprintf("connected in %d ms, slower than %d ms", latency.Milliseconds(), probe.Slow.Milliseconds())}
	}
	return Result{Status: Up, Latency: latency, HasLatency: true, Detail: fmt.Sprintf("connected in %d ms", latency.Milliseconds())}
}

// describe turns an error into a short detail that never holds a URL, query or credential.
func describe(err error) string {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		err = urlError.Err
	}

	var netError net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netError) && netError.Timeout():
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}

	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return "cannot resolve " + dnsError.Name
	}
	var syscallError *os.SyscallError
	if errors.As(err, &syscallError) {
		return syscallError.Err.Error()
	}
	var certificateError *tls.CertificateVerificationError
	if errors.As(err, &certificateError) {
		return "certificate not trusted"
	}

	detail := err.Error()
	if index := strings.LastIndex(detail, ": "); index >= 0 && strings.HasPrefix(detail, "dial ") {
		detail = detail[index+2:]
	}
	if len(detail) > 200 {
		detail = detail[:200]
	}
	return detail
}
