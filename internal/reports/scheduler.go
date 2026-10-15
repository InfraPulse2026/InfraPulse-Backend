package reports

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

type ScheduleInput struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Name             string `json:"name"`
	Enabled          bool   `json:"enabled"`
	Cadence          string `json:"cadence"`
	AssetID          string `json:"asset_id"`
	SiteID           string `json:"site_id"`
	Scope            string `json:"scope"`
	Confirmed        bool   `json:"confirmed"`
	Reason           string `json:"reason"`
}

func (v ScheduleInput) Validate() error {
	if v.ExpectedRevision < 0 || v.ExpectedRevision == int64(^uint64(0)>>1) || !v.Confirmed || len(strings.TrimSpace(v.Name)) < 1 || len(v.Name) > 128 || len(strings.TrimSpace(v.Reason)) < 5 || len(v.Reason) > 512 || strings.ContainsAny(v.Name+v.Reason, "\r\n\x00") || (v.Cadence != "daily" && v.Cadence != "weekly") {
		return ErrInvalid
	}
	_, err := (V2Filter{From: time.Now().Add(-time.Hour), To: time.Now(), AssetID: v.AssetID, SiteID: v.SiteID, Scope: v.Scope}).Normalize(time.Now())
	return err
}

type Schedule struct {
	ID             string        `json:"id"`
	Revision       int64         `json:"revision"`
	OwnerID        string        `json:"owner_id"`
	Definition     ScheduleInput `json:"definition"`
	NextRunAt      time.Time     `json:"next_run_at"`
	LastSnapshotID *string       `json:"last_snapshot_id,omitempty"`
	Failure        string        `json:"failure"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

// Completed UTC calendar periods. Missed historical periods are not fabricated.
func ClosedPeriod(now time.Time, cadence string) (time.Time, time.Time, time.Time) {
	day := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	length := 24 * time.Hour
	if cadence == "weekly" {
		day = day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
		length = 7 * 24 * time.Hour
	}
	return day.Add(-length), day, day.Add(length)
}

type ScheduledRepository interface {
	ProcessScheduledReports(context.Context, time.Time) (int, error)
}

func RunScheduled(ctx context.Context, repo ScheduledRepository, log *slog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, err := repo.ProcessScheduledReports(bounded, time.Now().UTC())
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Error("scheduled report generation failed")
			}
		}
	}
}
