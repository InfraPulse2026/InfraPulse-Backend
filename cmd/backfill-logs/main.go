// backfill-logs projects already archived logs without touching live sessions.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/config"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
)

func main() {
	limit := flag.Int("limit", 100, "unprojected batches to process, 1..1000")
	timeout := flag.Duration("timeout", 5*time.Minute, "maximum processing time")
	flag.Parse()
	if err := run(*limit, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(limit int, timeout time.Duration) error {
	if limit < 1 || limit > 1000 || timeout <= 0 {
		return fmt.Errorf("limit must be 1..1000 and timeout positive")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, stop := context.WithTimeout(ctx, timeout)
	defer stop()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := storage.Open(ctx, cfg.DatabaseURL, cfg.AgentTimeout, cfg.MonitoringDefaults, cfg.TelephonyIntervalSeconds)
	if err != nil {
		return err
	}
	defer db.Close()
	// The backend must have applied migration006. Do not call Migrate here:
	// its startup connection reset would disrupt a running controller.
	out, err := db.BackfillLogs(ctx, limit)
	if err != nil {
		return err
	}
	if err = json.NewEncoder(os.Stdout).Encode(out); err != nil {
		return err
	}
	if out.Failed > 0 {
		return fmt.Errorf("%d batches remain unprojected; inspect archived data", out.Failed)
	}
	return nil
}
