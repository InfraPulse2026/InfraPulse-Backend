package storage

import (
	"context"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/collectorcontract"
	"google.golang.org/protobuf/encoding/protojson"
)

func (s *Store) PendingAssetBackfill(ctx context.Context) (int64, error) {
	var count int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM agent_messages m WHERE m.kind IN ('hello','inventory') AND NOT EXISTS(SELECT 1 FROM asset_projection_marks p WHERE p.agent_id=m.agent_id AND p.message_id=m.message_id)`).Scan(&count)
	return count, err
}
func validAssetArchive(e *pb.AgentEnvelope, agent, message, kind string) bool {
	if e.AgentId != agent || e.MessageId != message || e.SessionId == "" || e.SentAt == nil || e.SentAt.CheckValid() != nil || Kind(e) != kind || collectorcontract.Validate(e) != nil {
		return false
	}
	if h := e.GetHello(); h != nil {
		return h.Device != nil && h.Device.Id != ""
	}
	if inv := e.GetInventory(); inv != nil {
		if inv.ObservedAt == nil || inv.ObservedAt.CheckValid() != nil {
			return false
		}
		for _, d := range inv.Devices {
			if d == nil || d.Id == "" {
				return false
			}
		}
		return true
	}
	return false
}

// BackfillAssets reads original archived envelopes and only writes the derived
// registry. It never calls Migrate, changes sessions or touches legacy devices.
func (s *Store) BackfillAssets(ctx context.Context, limit int) (assets.BackfillResult, error) {
	out := assets.BackfillResult{}
	if limit < 1 || limit > 200 {
		return out, assets.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT m.agent_id,m.message_id,m.kind,m.payload FROM agent_messages m WHERE m.kind IN ('hello','inventory') AND NOT EXISTS(SELECT 1 FROM asset_projection_marks p WHERE p.agent_id=m.agent_id AND p.message_id=m.message_id) ORDER BY m.received_at,m.agent_id,m.message_id LIMIT $1`, limit)
	if err != nil {
		return out, err
	}
	// Keep the reader streaming; at most one envelope is retained by the writer.
	for rows.Next() {
		var agent, message, kind string
		var raw []byte
		if err = rows.Scan(&agent, &message, &kind, &raw); err != nil {
			rows.Close()
			return out, err
		}
		out.Scanned++
		e := &pb.AgentEnvelope{}
		if protojson.Unmarshal(raw, e) != nil || !validAssetArchive(e, agent, message, kind) {
			out.Failed++
			continue
		}
		tx, e2 := s.pool.Begin(ctx)
		if e2 != nil {
			rows.Close()
			return out, e2
		}
		if _, e2 = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194030)`); e2 == nil {
			var done bool
			e2 = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM asset_projection_marks WHERE agent_id=$1 AND message_id=$2)`, agent, message).Scan(&done)
			if e2 == nil && !done {
				e2 = s.projectCollectorAssets(ctx, tx, e)
			}
		}
		if e2 == nil {
			e2 = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(context.Background())
		}
		if e2 != nil {
			out.Failed++
			if ctx.Err() != nil {
				rows.Close()
				return out, ctx.Err()
			}
		} else {
			out.Processed++
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	out.Remaining, err = s.PendingAssetBackfill(ctx)
	return out, err
}
