// backfill-assets projects archived inventory without reconnecting collectors.
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
	apply := flag.Bool("apply", false, "write the derived registry; default only counts pending envelopes")
	limit := flag.Int("limit", 50, "envelopes per run, 1..200")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall time limit")
	flag.Parse()
	if err := run(*apply, *limit, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(apply bool, limit int, timeout time.Duration) error {
	if limit < 1 || limit > 200 || timeout <= 0 || timeout > time.Hour {
		return fmt.Errorf("limit must be 1..200 and timeout 1ns..1h")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := storage.Open(ctx, cfg.DatabaseURL, cfg.AgentTimeout, cfg.MonitoringDefaults, cfg.TelephonyIntervalSeconds)
	if err != nil {
		return fmt.Errorf("cannot connect to backfill database")
	}
	defer db.Close()
	db.ConfigureAssetResolution(cfg.AssetAutoResolution, cfg.AssetResolutionMaxAge)
	// Migration022 must already be applied by the backend. Its session reset
	// must not be executed by this maintenance command on a running controller.
	if !apply {
		count, e := db.PendingAssetBackfill(ctx)
		if e != nil {
			return fmt.Errorf("cannot inspect asset backfill; apply backend migration022 first")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"apply": false, "pending": count})
	}
	result, err := db.BackfillAssets(ctx, limit)
	if err != nil {
		return fmt.Errorf("asset backfill failed; check database availability and migration022")
	}
	if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return err
	}
	if result.Failed > 0 {
		return fmt.Errorf("%d envelopes could not be projected; originals retained", result.Failed)
	}
	return nil
}
