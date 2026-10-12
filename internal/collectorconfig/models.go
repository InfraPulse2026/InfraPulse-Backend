package collectorconfig

import (
	"context"
	"encoding/json"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"log/slog"
	"time"
)

type Input struct {
	ExpectedRevision int64           `json:"expected_revision"`
	Confirmed        bool            `json:"confirmed"`
	AllowEmpty       bool            `json:"allow_empty_snapshot"`
	Reason           string          `json:"reason"`
	Site             string          `json:"site"`
	Targets          json.RawMessage `json:"targets"`
}
type Revision struct {
	CollectorID string          `json:"collector_id"`
	Revision    int64           `json:"revision"`
	Digest      string          `json:"configuration_digest"`
	Snapshot    json.RawMessage `json:"snapshot"`
	Site        string          `json:"site"`
	ActorID     string          `json:"actor_id"`
	Reason      string          `json:"reason"`
	CreatedAt   time.Time       `json:"created_at"`
}
type State struct {
	CollectorID             string     `json:"collector_id"`
	Enabled                 bool       `json:"backend_enabled"`
	Connected               bool       `json:"connected"`
	ManagedConfigV1         bool       `json:"managed_config_v1"`
	RemoteManagementEnabled bool       `json:"remote_management_enabled"`
	SessionID               string     `json:"session_id"`
	Desired                 *Revision  `json:"desired"`
	AppliedRevision         int64      `json:"applied_revision"`
	AppliedDigest           string     `json:"applied_digest"`
	AppliedAt               *time.Time `json:"applied_at"`
	VerifiedSessionID       string     `json:"verified_session_id"`
	Status                  string     `json:"status"`
	StatusRevision          int64      `json:"status_revision"`
	ErrorCode               string     `json:"error_code"`
	HighestReportedRevision string     `json:"highest_reported_revision"`
	InSync                  bool       `json:"in_sync"`
}
type Event struct {
	ID          int64     `json:"id"`
	CollectorID string    `json:"collector_id"`
	SessionID   string    `json:"session_id"`
	Revision    string    `json:"revision"`
	Digest      string    `json:"configuration_digest"`
	Status      string    `json:"status"`
	Correlated  bool      `json:"correlated"`
	ErrorCode   string    `json:"error_code"`
	CreatedAt   time.Time `json:"created_at"`
}
type Delivery struct {
	CollectorID string
	SessionID   string
	Snapshot    *pb.DesiredConfigSnapshot
}
type Repository interface {
	PendingCollectorConfigs(context.Context, int) ([]Delivery, error)
	PrepareCollectorConfig(context.Context, Delivery) (bool, error)
}
type Sender interface {
	HasManagedSession(string, string) bool
	SendDesiredConfig(context.Context, string, string, *pb.DesiredConfigSnapshot) (bool, error)
}

// Each full snapshot is idempotent by (revision,digest). Persist intent before
// Send so a fast ConfigApplyStatus can already correlate to this delivery.
func Dispatch(ctx context.Context, repo Repository, sender Sender, log *slog.Logger) {
	items, err := repo.PendingCollectorConfigs(ctx, 16)
	if err != nil {
		if ctx.Err() == nil {
			log.Error("cannot read Collector configuration queue", "error", err)
		}
		return
	}
	for _, d := range items {
		if ctx.Err() != nil {
			return
		}
		if !sender.HasManagedSession(d.CollectorID, d.SessionID) {
			continue
		}
		ready, err := repo.PrepareCollectorConfig(ctx, d)
		if err != nil {
			log.Error("cannot prepare Collector configuration", "collector_id", d.CollectorID, "error", err)
			continue
		}
		if !ready {
			continue
		}
		_, err = sender.SendDesiredConfig(ctx, d.CollectorID, d.SessionID, d.Snapshot)
		if err != nil && ctx.Err() == nil {
			log.Warn("Collector configuration delivery awaits retry", "collector_id", d.CollectorID, "revision", d.Snapshot.Revision)
		}
	}
}
func Run(ctx context.Context, repo Repository, sender Sender, log *slog.Logger) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			Dispatch(ctx, repo, sender, log)
		}
	}
}
