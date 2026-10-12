package topology

import (
	"context"
	"log/slog"
	"time"
)

type ExpiryRepository interface {
	ExpireGlobalTopology(context.Context, time.Time) error
}

func RunExpiry(ctx context.Context, repo ExpiryRepository, log *slog.Logger) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		step, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := repo.ExpireGlobalTopology(step, time.Now().UTC())
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Error("topology expiry failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
