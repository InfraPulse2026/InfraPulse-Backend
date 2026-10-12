package storage

import (
	"context"
	"errors"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/telephony"
	"github.com/jackc/pgx/v5"
)

var ErrInvalidTelephony = errors.New("invalid telephony update")

func projectTelephony(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope) error {
	u := e.GetTelephony()
	if err := telephony.Validate(u); err != nil {
		return ErrInvalidTelephony
	}
	var received time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&received); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO telephony_batches(agent_id,message_id,observed_at,source,received_at,target_count) VALUES($1,$2,$3,$4,$5,$6)`, e.AgentId, e.MessageId, at(u.ObservedAt, e.SentAt.AsTime()), u.Source, received, len(u.Targets)); err != nil {
		return err
	}
	// One lock per target; a SIP target is not a device and cannot change device health.
	for _, h := range u.Targets {
		if _, err := tx.Exec(ctx, `INSERT INTO telephony_targets(agent_id,target_id,first_received_at,last_received_at)
  VALUES($1,$2,$3,$3) ON CONFLICT(agent_id,target_id) DO UPDATE SET last_received_at=GREATEST(telephony_targets.last_received_at,EXCLUDED.last_received_at)`, e.AgentId, h.TargetId, received); err != nil {
			return err
		}
		observed := h.ObservedAt.AsTime().UTC()
		skew := observed.After(received.Add(30 * time.Second))
		var id int64
		err := tx.QueryRow(ctx, `INSERT INTO telephony_samples(agent_id,target_id,message_id,address,transport,source,reachable,response_code,latency_ms,observed_at,received_at,consecutive_failures,state,error_category,clock_skew)
  VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`, e.AgentId, h.TargetId, e.MessageId, h.Address, h.Transport, h.Source, h.Reachable, h.ResponseCode, h.LatencyMs, observed, received, h.ConsecutiveFailures, h.State, h.ErrorCategory, skew).Scan(&id)
		if err != nil {
			return err
		}
		if skew {
			_, err = tx.Exec(ctx, `UPDATE telephony_targets t SET clock_issue_sample_id=$3 WHERE t.agent_id=$1 AND t.target_id=$2
   AND (t.clock_issue_sample_id IS NULL OR $4 >= (SELECT received_at FROM telephony_samples WHERE id=t.clock_issue_sample_id))`, e.AgentId, h.TargetId, id, received)
		} else {
			_, err = tx.Exec(ctx, `UPDATE telephony_targets t SET latest_sample_id=$3 WHERE t.agent_id=$1 AND t.target_id=$2
   AND (t.latest_sample_id IS NULL OR ($4,$5,$3)>(SELECT observed_at,received_at,id FROM telephony_samples WHERE id=t.latest_sample_id))`, e.AgentId, h.TargetId, id, observed, received)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) telephonyDefault() int {
	if s.telephonyInterval == 0 {
		return telephony.DefaultIntervalSeconds
	}
	return s.telephonyInterval
}

func (s *Store) UpdateTelephonyMonitoring(ctx context.Context, agent, target string, interval int) error {
	if err := telephony.ValidateInterval(interval); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	result, err := tx.Exec(ctx, `INSERT INTO telephony_policies(agent_id,target_id,expected_interval_seconds)
 SELECT agent_id,target_id,$3 FROM telephony_targets WHERE agent_id=$1 AND target_id=$2
 ON CONFLICT(agent_id,target_id) DO UPDATE SET expected_interval_seconds=EXCLUDED.expected_interval_seconds,updated_at=now(),origin='manual',managed_revision=NULL`, agent, target, interval)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err = monitoringAccessTx(ctx, tx, "sip:"+agent+":"+target); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
