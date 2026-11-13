package runner

import (
	"context"
	"time"

	"github.com/alimrun/autoheal-agent/internal/api"
	"github.com/alimrun/autoheal-agent/internal/backoff"
	"github.com/alimrun/autoheal-agent/internal/recovery"
)

// Recovery is what the recovery loop needs. The runner asks for tasks only when it is set.
type Recovery struct {
	Verifier *recovery.Verifier
	Replay   *recovery.ReplayStore
	Registry *recovery.Registry
}

// taskPolling is what the recovery loop remembers between two fetches.
type taskPolling struct {
	policy      *backoff.Policy
	paused      bool
	unsupported bool
	// rotated is set once a rotation was tried for a missing scope, until a fetch works.
	rotated bool
	// refused is the last task whose refusal the platform answered, so that a task it keeps
	// offering is reported once.
	refused string
}

// recoveryLoop fetches recovery tasks and handles them one at a time until ctx is cancelled.
func (runner *Runner) recoveryLoop(ctx context.Context) {
	polling := &taskPolling{policy: backoff.New(runner.options.BackoffBase, runner.options.BackoffMax)}
	for {
		wait := runner.pollTasks(ctx, polling)
		if !runner.sleep(ctx, wait, false) {
			return
		}
	}
}

// pollTasks makes one fetch, handles what it returns and says how long to wait before the next.
func (runner *Runner) pollTasks(ctx context.Context, polling *taskPolling) time.Duration {
	token, refused := runner.currentToken()
	if refused {
		return runner.options.CredentialRecheck
	}

	answer, err := runner.client.GetTasks(ctx, token)
	if ctx.Err() != nil {
		return 0
	}
	if err != nil {
		return runner.taskFetchFailed(ctx, polling, token, err)
	}

	if polling.policy.Failures() > 0 {
		runner.log.Info("recovery tasks can be fetched again", "failed_attempts", polling.policy.Failures())
	}
	polling.policy.Reset()
	polling.unsupported, polling.rotated = false, false

	if answer.Paused != polling.paused {
		polling.paused = answer.Paused
		if answer.Paused {
			runner.log.Info("AutoHeal paused recovery; no task is acted on until it resumes")
		} else {
			runner.log.Info("AutoHeal resumed recovery")
		}
	}
	if !answer.Paused {
		runner.handleTasks(ctx, polling, answer.Tasks)
	}
	return runner.options.TaskPoll
}

func (runner *Runner) taskFetchFailed(ctx context.Context, polling *taskPolling, token string, err error) time.Duration {
	switch api.KindOf(err) {
	case api.NotFound:
		if !polling.unsupported {
			runner.log.Info("this AutoHeal has no recovery support; asking again at a slower pace", "every", runner.options.TaskPollUnsupported.String())
		}
		polling.unsupported = true
		return runner.options.TaskPollUnsupported
	case api.Unauthenticated:
		runner.markRefused(token, err)
		return 0
	case api.Scope:
		if !polling.rotated {
			runner.log.Info("the credential lacks the recovery scope; rotating it once")
			runner.rotate(ctx, token)
			if current, _ := runner.currentToken(); current != token {
				polling.rotated = true
				return 0
			}
		}
		if polling.policy.Failures() == 0 {
			runner.log.Error("the credential still lacks the recovery scope; no task is fetched until AutoHeal grants it or the agent is enrolled again with `autoheal-agent enroll --force`", "error", err.Error())
		}
	default:
		if polling.policy.Failures() == 0 {
			runner.log.Warn("fetching recovery tasks failed; retrying with backoff", "error", err.Error())
		}
	}
	return polling.policy.Next(api.RetryAfterOf(err))
}

func (runner *Runner) identity() recovery.Identity {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return recovery.Identity{AgentID: runner.credential.Agent.ID, EnvironmentID: runner.credential.Environment.ID}
}

// handleTasks holds at most one task: the first that verifies. Any other is refused as busy.
func (runner *Runner) handleTasks(ctx context.Context, polling *taskPolling, envelopes []api.TaskEnvelope) {
	var held *recovery.Task
	for _, envelope := range envelopes {
		task, refusal := runner.recovery.Verifier.Verify(envelope, runner.identity())
		if refusal == nil && held != nil {
			refusal = &recovery.Refusal{Code: recovery.RefusalBusy, TaskID: task.ID, Nonce: task.Nonce}
		}
		if refusal != nil {
			runner.refuseTask(ctx, polling, refusal)
			continue
		}
		if runner.receiveTask(ctx, task) {
			held = task
		}
	}
	if held != nil {
		runner.runTask(ctx, held)
	}
}

func (runner *Runner) refuseTask(ctx context.Context, polling *taskPolling, refusal *recovery.Refusal) {
	if !refusal.Reportable() {
		runner.log.Warn("dropped a recovery task that cannot be reported", "reason", refusal.Code)
		return
	}
	if polling.refused == refusal.TaskID {
		return
	}
	runner.log.Warn("refused a recovery task", "task", refusal.TaskID, "reason", refusal.Code)
	err := runner.reportTask(ctx, refusal.TaskID, api.TaskReport{Nonce: refusal.Nonce, Stage: api.StageRefused, ReasonCode: refusal.Code})
	if err == nil || !retryable(err) {
		polling.refused = refusal.TaskID
	}
}

// receiveTask remembers the task before reporting it received, so a crash between the two can
// never let it run twice. It returns whether the task may go on.
func (runner *Runner) receiveTask(ctx context.Context, task *recovery.Task) bool {
	if err := runner.recovery.Replay.Remember(task, time.Now()); err != nil {
		runner.log.Error("could not save the replay store; the recovery task is not acted on", "task", task.ID, "error", err.Error())
		return false
	}
	return runner.reportTask(ctx, task.ID, api.TaskReport{Nonce: task.Nonce, Stage: api.StageReceived}) == nil
}

// runTask acts only once the platform accepted the running report, then reports how the action
// ended. The verification check runs only after the platform accepted a finished report: it
// verifies a step from a check result dated at or after the time it recorded that report.
func (runner *Runner) runTask(ctx context.Context, task *recovery.Task) {
	if runner.reportTask(ctx, task.ID, api.TaskReport{Nonce: task.Nonce, Stage: api.StageRunning}) != nil {
		return
	}
	runner.log.Info("running a recovery task", "task", task.ID, "action", task.Action, "target", task.Target)

	outcome := runner.recovery.Registry.Run(ctx, task, runner)
	report := api.TaskReport{Nonce: task.Nonce, Stage: api.StageFinished, Evidence: &outcome.Evidence}
	if outcome.Failure != "" {
		report.Stage, report.ReasonCode = api.StageFailed, outcome.Failure
	}

	if ctx.Err() != nil {
		if outcome.Failure != "" {
			outcome.Evidence.Outcome = "agent stopped during the action"
		}
		runner.reportStopped(task, report)
		return
	}

	delivered := runner.reportTask(ctx, task.ID, report) == nil
	runner.log.Info("recovery task ended", "task", task.ID, "stage", report.Stage, "reason", report.ReasonCode, "outcome", outcome.Evidence.Outcome)
	if !delivered || outcome.Failure != "" {
		return
	}

	if _, err := runner.RunNow(ctx, task.VerificationCheck); err != nil && ctx.Err() == nil {
		runner.log.Warn("could not run the verification check; AutoHeal will not see a fresh result", "task", task.ID, "check", task.VerificationCheck, "error", err.Error())
	}
}

// reportStopped makes one short attempt, after shutdown began, to say how an interrupted task ended.
func (runner *Runner) reportStopped(task *recovery.Task, report api.TaskReport) {
	ctx, cancel := context.WithTimeout(context.Background(), runner.options.ShutdownSend)
	defer cancel()
	token, _ := runner.currentToken()
	if _, err := runner.client.ReportTask(ctx, token, task.ID, report); err != nil {
		runner.log.Warn("could not report a recovery task interrupted by shutdown", "task", task.ID, "stage", report.Stage, "error", err.Error())
	}
}

func retryable(err error) bool {
	kind := api.KindOf(err)
	return kind == api.Transient || kind == api.RateLimited
}

// reportTask delivers one report, retrying failures that may pass until the report window
// closes. A refused transition is never resent; it counts as delivered only when it shows that
// an earlier attempt, whose answer was lost, already arrived.
func (runner *Runner) reportTask(ctx context.Context, taskID string, report api.TaskReport) error {
	policy := backoff.New(runner.options.BackoffBase, runner.options.BackoffMax)
	deadline := time.Now().Add(runner.options.TaskReportWindow)
	for {
		token, _ := runner.currentToken()
		status, err := runner.client.ReportTask(ctx, token, taskID, report)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if api.KindOf(err) == api.Conflict && policy.Failures() > 0 && status == report.Stage {
			return nil
		}

		wait := policy.Next(api.RetryAfterOf(err))
		if !retryable(err) || time.Now().Add(wait).After(deadline) {
			runner.log.Warn("AutoHeal did not accept a recovery task report", "task", taskID, "stage", report.Stage, "error", err.Error())
			return err
		}
		if !runner.sleep(ctx, wait, false) {
			return ctx.Err()
		}
	}
}
