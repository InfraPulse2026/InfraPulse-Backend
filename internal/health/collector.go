package health

import (
	"context"
	"log/slog"
	"time"
)

type CollectorEvidence struct {
	CheckedAt           time.Time  `json:"checked_at"`
	LastSeenAt          time.Time  `json:"last_seen_at"`
	Connected           bool       `json:"connected"`
	AgentTimeoutSeconds float64    `json:"agent_timeout_seconds"`
	LossSince           *time.Time `json:"loss_since"`
	RecoverySince       *time.Time `json:"recovery_since"`
	ConfirmationSeconds int        `json:"confirmation_seconds"`
}
type CollectorRepository interface {
	SweepCollectors(context.Context, time.Time, string) (string, error)
}

func RunCollectors(ctx context.Context, repo CollectorRepository, log *slog.Logger) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	cursor := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
			next, err := repo.SweepCollectors(bounded, time.Now().UTC(), cursor)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					log.Error("collector health sweep failed", "error", err)
				}
			} else {
				cursor = next
			}
		}
	}
}
