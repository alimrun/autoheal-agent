// Package runner runs the agent: heartbeats, checks, batching, sending and shutdown.
package runner

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alimrun/autoheal-agent/internal/api"
	"github.com/alimrun/autoheal-agent/internal/backoff"
	"github.com/alimrun/autoheal-agent/internal/buildinfo"
	"github.com/alimrun/autoheal-agent/internal/check"
	"github.com/alimrun/autoheal-agent/internal/credential"
	"github.com/alimrun/autoheal-agent/internal/event"
	"github.com/alimrun/autoheal-agent/internal/spool"
)

type Options struct {
	BatchSize         int
	FlushInterval     time.Duration
	BackoffBase       time.Duration
	BackoffMax        time.Duration
	CredentialRecheck time.Duration
	ShutdownSend      time.Duration
	MinHeartbeat      time.Duration
	MaxHeartbeat      time.Duration
	MaxAge            time.Duration
	SkewLimit         time.Duration
	WarnEvery         time.Duration
	Confirm           int
	Refresh           time.Duration
	QueueSize         int
	CheckJitter       time.Duration
}

func DefaultOptions() Options {
	return Options{
		BatchSize:         50,
		FlushInterval:     10 * time.Second,
		BackoffBase:       time.Second,
		BackoffMax:        5 * time.Minute,
		CredentialRecheck: 5 * time.Minute,
		ShutdownSend:      5 * time.Second,
		MinHeartbeat:      10 * time.Second,
		MaxHeartbeat:      time.Hour,
		MaxAge:            23 * time.Hour,
		SkewLimit:         2 * time.Minute,
		WarnEvery:         time.Hour,
		Confirm:           2,
		Refresh:           5 * time.Minute,
		QueueSize:         500,
		CheckJitter:       10 * time.Second,
	}
}

type Sender interface {
	Heartbeat(ctx context.Context, token string, details api.Details) (*api.HeartbeatAnswer, error)
	Rotate(ctx context.Context, token string) (*api.IssuedCredential, error)
	SendEvents(ctx context.Context, token string, events []json.RawMessage) (*api.Accepted, error)
}

type Deps struct {
	Options    Options
	Log        *slog.Logger
	Client     Sender
	StateDir   string
	Details    api.Details
	Checks     []*check.Check
	Spool      *spool.Spool
	Credential *credential.Credential
	Stamp      credential.Stamp
}

type Runner struct {
	options  Options
	log      *slog.Logger
	client   Sender
	stateDir string
	details  api.Details
	checks   []*check.Check
	spool    *spool.Spool

	mu           sync.Mutex
	credential   *credential.Credential
	stamp        credential.Stamp
	refusedToken string

	events         chan event.Event
	wake           chan struct{}
	droppedReports atomic.Int64

	lastMinimumWarning time.Time
	lastSkewWarning    time.Time
}

func New(deps Deps) *Runner {
	return &Runner{
		options:    deps.Options,
		log:        deps.Log,
		client:     deps.Client,
		stateDir:   deps.StateDir,
		details:    deps.Details,
		checks:     deps.Checks,
		spool:      deps.Spool,
		credential: deps.Credential,
		stamp:      deps.Stamp,
		events:     make(chan event.Event, deps.Options.QueueSize),
		wake:       make(chan struct{}, 1),
	}
}

// Run works until ctx is cancelled, then saves waiting events and makes one last, short send.
func (runner *Runner) Run(ctx context.Context) {
	runner.log.Info("agent started",
		"version", runner.details.Version,
		"agent", runner.credential.Agent.Name,
		"project", runner.credential.Project.Name,
		"environment", runner.credential.Environment.Name,
		"checks", len(runner.checks),
		"spooled_events", runner.spool.Len())

	var producers sync.WaitGroup
	for _, configured := range runner.checks {
		producers.Add(1)
		go func() {
			defer producers.Done()
			runner.runCheck(ctx, configured)
		}()
	}

	var workers sync.WaitGroup
	workers.Add(3)
	go func() { defer workers.Done(); runner.batch(ctx, &producers) }()
	go func() { defer workers.Done(); runner.sendLoop(ctx) }()
	go func() { defer workers.Done(); runner.heartbeatLoop(ctx) }()

	<-ctx.Done()
	workers.Wait()
	runner.finalSend()
	runner.log.Info("agent stopped", "spooled_events", runner.spool.Len())
}

func (runner *Runner) notify() {
	select {
	case runner.wake <- struct{}{}:
	default:
	}
}

// sleep waits for d, or until woken when wake is true. It returns false once ctx is done.
func (runner *Runner) sleep(ctx context.Context, wait time.Duration, wake bool) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var woken <-chan struct{}
	if wake {
		woken = runner.wake
	}
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
	case <-woken:
	}
	return true
}

func (runner *Runner) runCheck(ctx context.Context, configured *check.Check) {
	if runner.options.CheckJitter > 0 {
		offset := time.Duration(rand.Int64N(int64(min(configured.Interval, runner.options.CheckJitter))))
		if !runner.sleep(ctx, offset, false) {
			return
		}
	}

	reporter := check.NewReporter(runner.options.Confirm, runner.options.Refresh)
	ticker := time.NewTicker(configured.Interval)
	defer ticker.Stop()

	for {
		result := configured.Run(ctx)
		if ctx.Err() != nil {
			return
		}
		now := time.Now()
		if reporter.Observe(result, now) {
			runner.report(configured.Event(result, now, runner.details.Version))
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (runner *Runner) report(built event.Event) {
	select {
	case runner.events <- built:
	default:
		if runner.droppedReports.Add(1) == 1 {
			runner.log.Warn("events are arriving faster than they can be saved; dropping results", "check", built.Body.Name)
		}
	}
}

func (runner *Runner) batch(ctx context.Context, producers *sync.WaitGroup) {
	var pending []event.Event
	var timer *time.Timer
	var deadline <-chan time.Time

	flush := func() {
		if timer != nil {
			timer.Stop()
			timer, deadline = nil, nil
		}
		if len(pending) == 0 {
			return
		}
		if err := runner.spool.Append(pending); err != nil {
			runner.log.Error("could not save events to the spool; they are lost", "events", len(pending), "error", err.Error())
		}
		pending = nil
		runner.notify()
	}
	add := func(built event.Event) {
		pending = append(pending, built)
		if len(pending) == 1 {
			timer = time.NewTimer(runner.options.FlushInterval)
			deadline = timer.C
		}
		if len(pending) >= runner.options.BatchSize {
			flush()
		}
	}

	for {
		select {
		case built := <-runner.events:
			add(built)
		case <-deadline:
			flush()
		case <-ctx.Done():
			producers.Wait()
			for {
				select {
				case built := <-runner.events:
					add(built)
				default:
					flush()
					return
				}
			}
		}
	}
}

func (runner *Runner) currentToken() (token string, refused bool) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	// Only the token that was refused counts as refused: a rotation answer can arrive after a
	// send with the old token was refused, and the new token must then still be used.
	return runner.credential.Token, runner.refusedToken != "" && runner.refusedToken == runner.credential.Token
}

func (runner *Runner) markRefused(token string, err error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.credential.Token != token || runner.refusedToken == token {
		return
	}
	runner.refusedToken = token
	runner.log.Error("AutoHeal refused this agent's credential; nothing will be sent until it is enrolled again with `autoheal-agent enroll --force`",
		"reason", api.KindOf(err).String(), "error", err.Error(), "spooled_events", runner.spool.Len())
}

// reloadCredential picks up a credential that enroll or a rotation wrote since the last look.
func (runner *Runner) reloadCredential() {
	stamp, err := credential.StampOf(runner.stateDir)
	runner.mu.Lock()
	unchanged := err != nil || stamp == runner.stamp
	runner.mu.Unlock()
	if unchanged {
		return
	}

	loaded, loadedStamp, err := credential.Load(runner.stateDir)
	if err != nil {
		runner.log.Warn("the credential file changed but cannot be used", "error", err.Error())
		return
	}

	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.stamp = loadedStamp
	if loaded.APIURL != runner.credential.APIURL {
		runner.log.Error("the agent was enrolled with a different API address; restart it to use the new one", "api_url", loaded.APIURL)
		return
	}
	if loaded.Token == runner.credential.Token {
		return
	}
	wasRefused := runner.refusedToken != "" && runner.refusedToken == runner.credential.Token
	runner.credential = loaded
	runner.refusedToken = ""
	if wasRefused {
		runner.log.Info("found a new credential; resuming", "agent", loaded.Agent.Name, "environment", loaded.Environment.Name)
	}
	runner.notify()
}

func (runner *Runner) heartbeatLoop(ctx context.Context) {
	policy := backoff.New(runner.options.BackoffBase, runner.options.BackoffMax)
	for {
		runner.reloadCredential()
		token, refused := runner.currentToken()
		if refused {
			if !runner.sleep(ctx, runner.options.CredentialRecheck, false) {
				return
			}
			continue
		}

		answer, err := runner.client.Heartbeat(ctx, token, runner.details)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			kind := api.KindOf(err)
			if kind == api.Unauthenticated || kind == api.Scope {
				runner.markRefused(token, err)
				continue
			}
			if policy.Failures() == 0 {
				runner.log.Warn("heartbeat failed; retrying with backoff", "error", err.Error())
			}
			if !runner.sleep(ctx, policy.Next(api.RetryAfterOf(err)), false) {
				return
			}
			continue
		}

		if policy.Failures() > 0 {
			runner.log.Info("heartbeat delivered again", "failed_attempts", policy.Failures())
		}
		policy.Reset()
		runner.warnMinimum(answer.MinimumVersion)
		runner.warnSkew(answer.ServerTime)
		if answer.Credential.Rotate {
			runner.rotate(ctx, token)
		}

		interval := time.Duration(answer.HeartbeatInterval) * time.Second
		interval = min(max(interval, runner.options.MinHeartbeat), runner.options.MaxHeartbeat)
		if !runner.sleep(ctx, interval, false) {
			return
		}
	}
}

// rotate saves the new credential before using it, so a crash can never strand the agent;
// until then the old one keeps working through the platform's grace period.
func (runner *Runner) rotate(ctx context.Context, token string) {
	issued, err := runner.client.Rotate(ctx, token)
	if err != nil {
		runner.log.Warn("credential rotation failed; the next heartbeat will try again", "error", err.Error())
		return
	}

	runner.mu.Lock()
	next := *runner.credential
	runner.mu.Unlock()
	next.Token = issued.Token
	next.ExpiresAt = issued.ExpiresAt

	stamp, err := credential.Save(runner.stateDir, &next)
	if err != nil {
		runner.log.Error("could not save the rotated credential; keeping the current one", "error", err.Error())
		return
	}

	runner.mu.Lock()
	runner.credential = &next
	runner.stamp = stamp
	runner.mu.Unlock()
	runner.log.Info("credential rotated", "expires_at", issued.ExpiresAt.Format(time.RFC3339))
}

func (runner *Runner) warnMinimum(minimum string) {
	if minimum == "" {
		return
	}
	below, ok := buildinfo.Below(runner.details.Version, minimum)
	if !ok {
		runner.log.Debug("ignoring a minimum version that is not a version number", "minimum_version", minimum)
		return
	}
	if below && time.Since(runner.lastMinimumWarning) >= runner.options.WarnEvery {
		runner.lastMinimumWarning = time.Now()
		runner.log.Warn("this agent is older than the minimum version AutoHeal supports; upgrade it",
			"version", runner.details.Version, "minimum_version", minimum)
	}
}

func (runner *Runner) warnSkew(serverTime time.Time) {
	if serverTime.IsZero() {
		return
	}
	skew := time.Since(serverTime)
	if skew < 0 {
		skew = -skew
	}
	if skew > runner.options.SkewLimit && time.Since(runner.lastSkewWarning) >= runner.options.WarnEvery {
		runner.lastSkewWarning = time.Now()
		runner.log.Warn("this host's clock differs from AutoHeal's; events dated more than 5 minutes ahead are refused",
			"local_time", time.Now().UTC().Format(time.RFC3339), "autoheal_time", serverTime.UTC().Format(time.RFC3339))
	}
}

func (runner *Runner) sendLoop(ctx context.Context) {
	policy := backoff.New(runner.options.BackoffBase, runner.options.BackoffMax)
	for ctx.Err() == nil {
		token, refused := runner.currentToken()
		if refused {
			if !runner.sleep(ctx, runner.options.CredentialRecheck, true) {
				return
			}
			// Re-check here too: the heartbeat loop may be in a sleep of up to MaxHeartbeat.
			runner.reloadCredential()
			continue
		}

		batch := runner.nextBatch()
		if batch == nil {
			if !runner.sleep(ctx, runner.options.CredentialRecheck, true) {
				return
			}
			continue
		}

		if wait := runner.deliver(ctx, token, batch, policy); wait > 0 {
			if !runner.sleep(ctx, wait, false) {
				return
			}
		}
	}
}

// nextBatch is the oldest batch with events past the platform's age limit removed.
func (runner *Runner) nextBatch() *spool.Batch {
	for {
		batch, err := runner.spool.Oldest()
		if err != nil {
			runner.log.Error("cannot read the spool", "error", err.Error())
			return nil
		}
		if batch == nil {
			return nil
		}

		cutoff := time.Now().Add(-runner.options.MaxAge)
		kept := batch.Events[:0:0]
		for _, waiting := range batch.Events {
			if waiting.OccurredAt.After(cutoff) {
				kept = append(kept, waiting)
			}
		}
		if len(kept) == len(batch.Events) {
			return batch
		}

		runner.log.Warn("dropping events too old for AutoHeal to accept", "dropped", len(batch.Events)-len(kept), "max_age", runner.options.MaxAge.String())
		if err := runner.spool.Replace(batch, kept); err != nil {
			runner.log.Error("cannot rewrite a spool batch", "error", err.Error())
			return nil
		}
		if len(kept) > 0 {
			return batch
		}
	}
}

// deliver sends one batch and returns how long to wait before the next attempt.
func (runner *Runner) deliver(ctx context.Context, token string, batch *spool.Batch, policy *backoff.Policy) time.Duration {
	encoded := make([]json.RawMessage, 0, len(batch.Events))
	for _, waiting := range batch.Events {
		raw, err := json.Marshal(waiting)
		if err != nil {
			runner.log.Error("cannot encode a spooled event", "error", err.Error())
			continue
		}
		encoded = append(encoded, raw)
	}

	accepted, err := runner.client.SendEvents(ctx, token, encoded)
	if err == nil {
		if removeErr := runner.spool.Remove(batch); removeErr != nil {
			runner.log.Error("cannot remove a delivered batch; it will be sent again and counted as duplicates", "error", removeErr.Error())
		}
		if policy.Failures() > 0 {
			runner.log.Info("telemetry delivered again", "failed_attempts", policy.Failures(), "spooled_events", runner.spool.Len())
		}
		policy.Reset()
		runner.log.Debug("batch delivered", "accepted", accepted.Accepted, "duplicates", accepted.Duplicates)
		return 0
	}
	if ctx.Err() != nil {
		return 0
	}

	switch api.KindOf(err) {
	case api.Rejected:
		var apiError *api.Error
		fields := ""
		if asAPIError(err, &apiError) {
			fields = apiError.FieldSummary()
		}
		runner.log.Error("AutoHeal refused a batch as invalid; dropping it", "events", len(batch.Events), "error", err.Error(), "fields", fields)
		runner.spool.Remove(batch)
		return 0
	case api.Unauthenticated, api.Scope:
		runner.markRefused(token, err)
		return 0
	default:
		if policy.Failures() == 0 {
			runner.log.Warn("sending telemetry failed; events stay in the spool and are retried with backoff", "error", err.Error(), "spooled_events", runner.spool.Len())
		}
		return policy.Next(api.RetryAfterOf(err))
	}
}

func asAPIError(err error, target **api.Error) bool {
	apiError, ok := err.(*api.Error)
	*target = apiError
	return ok
}

// finalSend tries, for a few seconds after shutdown, to deliver what is waiting.
func (runner *Runner) finalSend() {
	ctx, cancel := context.WithTimeout(context.Background(), runner.options.ShutdownSend)
	defer cancel()
	policy := backoff.New(runner.options.BackoffBase, runner.options.BackoffMax)

	for ctx.Err() == nil {
		token, refused := runner.currentToken()
		if refused {
			return
		}
		batch := runner.nextBatch()
		if batch == nil {
			return
		}
		if wait := runner.deliver(ctx, token, batch, policy); wait > 0 {
			return
		}
	}
}
