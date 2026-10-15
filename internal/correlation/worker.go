package correlation

import (
	"context"
	"log/slog"
	"time"
)

type Repository interface {
	ProcessCorrelation(context.Context) (int, error)
}

func Run(ctx context.Context, repo Repository, log *slog.Logger) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, err := repo.ProcessCorrelation(bounded)
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Error("correlation projection failed", "error", err)
			}
		}
	}
}
