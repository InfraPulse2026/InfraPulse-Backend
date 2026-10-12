package automation

import (
	"context"
	"log/slog"
	"time"
)

// Run consumes a durable queue. Separate heartbeats keep availability truthful
// during long jobs. Loss of DB connectivity cancels execution rather than
// permitting unaudited work to continue.
func Run(ctx context.Context, repo Repository, exec Executor, workerID string, log *slog.Logger) error {
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	status := exec.Probe(probeCtx)
	cancel()
	status.ID = workerID
	if err := repo.AutomationWorker(ctx, status); err != nil {
		return err
	}
	aliveCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-aliveCtx.Done():
				return
			case <-ticker.C:
				if err := repo.AutomationWorker(aliveCtx, status); err != nil {
					stop()
					return
				}
			}
		}
	}()
	defer func() {
		stop()
		<-done
		end, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		status.Available = false
		status.ErrorCode = "shutdown"
		_ = repo.AutomationWorker(end, status)
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-aliveCtx.Done():
			return aliveCtx.Err()
		case <-ticker.C:
			if !status.Available {
				continue
			}
			job, err := repo.ClaimAutomation(aliveCtx, workerID)
			if err != nil {
				log.Warn("automation queue unavailable")
				continue
			}
			if job.ID == "" {
				continue
			}
			state, result := executeJob(aliveCtx, repo, exec, job)
			finish, c := context.WithTimeout(context.Background(), 5*time.Second)
			err = repo.FinishAutomation(finish, job, state, result)
			c()
			if err != nil {
				log.Warn("automation result could not be persisted", "job_id", job.ID)
			} else {
				log.Info("automation job finished", "job_id", job.ID, "state", state)
			}
		}
	}
}
func executeJob(ctx context.Context, repo Repository, exec Executor, j Job) (string, Result) {
	valid, err := repo.AutomationRunning(ctx, j)
	if err != nil || !valid {
		return "failed", Result{ErrorCode: "authorization_or_target_changed"}
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(j.TimeoutSeconds)*time.Second)
	defer cancel()
	done := make(chan Result, 1)
	go func() { done <- exec.Execute(runCtx, j) }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	state := ""
	code := ""
	for {
		select {
		case r := <-done:
			if state != "" {
				return state, Result{ErrorCode: code}
			}
			if runCtx.Err() != nil {
				if ctx.Err() != nil {
					return "cancelled", Result{ErrorCode: "shutdown"}
				}
				return "timed_out", Result{ErrorCode: "timed_out"}
			}
			if r.ErrorCode != "" {
				if r.ErrorCode == "timed_out" {
					return "timed_out", r
				}
				if r.ErrorCode == "cancelled" {
					return "cancelled", r
				}
				return "failed", r
			}
			if r.Validate() != nil {
				return "failed", Result{ErrorCode: "invalid_result"}
			}
			return "succeeded", r
		case <-ticker.C:
			valid, err := repo.AutomationRunning(runCtx, j)
			if err != nil || !valid {
				state = "cancelled"
				code = "cancelled"
				cancel()
			}
		case <-runCtx.Done():
			if state == "" {
				if ctx.Err() != nil {
					state = "cancelled"
					code = "shutdown"
				} else {
					state = "timed_out"
					code = "timed_out"
				}
			}
			cancel()
			r := <-done
			_ = r
			return state, Result{ErrorCode: code}
		}
	}
}
