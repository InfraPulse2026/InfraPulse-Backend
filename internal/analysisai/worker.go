package analysisai

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

func Run(ctx context.Context, repo Repository, provider Provider, cfg Config, log *slog.Logger) {
	if !cfg.Enabled {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
			job, preview, err := repo.ClaimJob(bounded, cfg)
			cancel()
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				if ctx.Err() == nil {
					log.Error("AI job claim failed", "error", err)
				}
				continue
			}
			requestCtx, stop := context.WithTimeout(ctx, cfg.Timeout)
			result, err := provider.Analyze(requestCtx, preview)
			stop()
			failure := ""
			uncertain := false
			var accepted *Result
			if err != nil {
				failure = "analysis_failed"
				var p *ProviderError
				if errors.As(err, &p) {
					failure = p.Code
					uncertain = p.Uncertain
				}
			} else {
				accepted = &result
			}
			finish, done := context.WithTimeout(context.Background(), 3*time.Second)
			err = repo.FinishJob(finish, job.ID, accepted, failure, uncertain)
			done()
			if err != nil {
				log.Error("AI job result persistence failed", "job_id", job.ID)
			}
		}
	}
}
