package storage

import (
	"context"
	"errors"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/jackc/pgx/v5"
)

// SweepCollectors checks 100 agents per tick. Row locks serialize decisions
// with registration/heartbeat/disconnect; this does not mark downstream offline.
func (s *Store) SweepCollectors(ctx context.Context, now time.Time, cursor string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return cursor, err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx, `SELECT agent_id,session_id,connected,last_seen FROM agents WHERE agent_id>$1 ORDER BY agent_id LIMIT 100 FOR UPDATE`, cursor)
	if err != nil {
		return cursor, err
	}
	type item struct {
		id, session string
		connected   bool
		lastSeen    time.Time
	}
	agents := []item{}
	for rows.Next() {
		var v item
		if err = rows.Scan(&v.id, &v.session, &v.connected, &v.lastSeen); err != nil {
			rows.Close()
			return cursor, err
		}
		agents = append(agents, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return cursor, err
	}
	for _, agent := range agents {
		var loss, recovery *time.Time
		var previousSession string
		var previousAt time.Time
		err = tx.QueryRow(ctx, `SELECT loss_since,recovery_since,session_id,observed_at FROM collector_health_trackers WHERE agent_id=$1 FOR UPDATE`, agent.id).Scan(&loss, &recovery, &previousSession, &previousAt)
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return cursor, err
		}
		if exists && !now.After(previousAt) {
			continue
		}
		healthy := agent.connected && !agent.lastSeen.Before(now.Add(-s.timeout)) && !agent.lastSeen.After(now.Add(30*time.Second))
		grace := !s.collectorStartedAt.IsZero() && now.Before(s.collectorStartedAt.Add(max(15*time.Second, s.timeout)))
		a := health.Assessment{RuleID: "collector.unavailable", RuleVersion: "1", Policy: s.HealthPolicy(), Status: "unknown", Severity: "none", Reason: "startup_grace", ObservedAt: &now, Metric: "collector_connection", Unit: "boolean", Evidence: []health.Point{}}
		if healthy {
			loss = nil
			if recovery == nil || previousSession != agent.session {
				at := now
				recovery = &at
			}
			a.Status = "ok"
			a.Reason = "collector_connected"
			a.RecoveryEligible = now.Sub(*recovery) >= 15*time.Second && !agent.lastSeen.Before(recovery.Add(6*time.Second))
		} else {
			recovery = nil
			if grace {
				loss = nil
			} else {
				if loss == nil {
					at := now
					loss = &at
				}
				a.Status = "pending"
				a.Reason = "collector_loss_not_confirmed"
				if now.Sub(*loss) >= 15*time.Second {
					a.Status = "critical"
					a.Severity = "critical"
					a.Reason = "collector_connection_unavailable"
				}
			}
		}
		a.Collector = &health.CollectorEvidence{CheckedAt: now, LastSeenAt: agent.lastSeen, Connected: agent.connected, AgentTimeoutSeconds: s.timeout.Seconds(), LossSince: loss, RecoverySince: recovery, ConfirmationSeconds: 15}
		if _, err = tx.Exec(ctx, `INSERT INTO collector_health_trackers(agent_id,loss_since,recovery_since,session_id,observed_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(agent_id) DO UPDATE SET loss_since=EXCLUDED.loss_since,recovery_since=EXCLUDED.recovery_since,session_id=EXCLUDED.session_id,observed_at=EXCLUDED.observed_at`, agent.id, loss, recovery, agent.session, now); err != nil {
			return cursor, err
		}
		if err = saveResourceHealth(ctx, tx, agent.id, "", "controller", "", "agent", agent.id, "", a); err != nil {
			return cursor, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return cursor, err
	}
	if len(agents) < 100 {
		return "", nil
	}
	return agents[len(agents)-1].id, nil
}
