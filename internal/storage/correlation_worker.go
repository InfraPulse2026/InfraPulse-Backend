package storage

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// Queue in the same durable transaction as the source incident. Projection
// failure never loses the signal or makes the gRPC receive path expensive.
func queueIncidentCorrelation(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `INSERT INTO correlation_pending(incident_id) VALUES($1) ON CONFLICT(incident_id) DO UPDATE SET requested_at=now()`, id)
	return err
}

func (s *Store) ProcessCorrelation(ctx context.Context) (int, error) {
	if _, err := s.ProcessHardwareSignals(ctx); err != nil {
		return 0, err
	}
	if _, err := s.ProcessLogSignals(ctx); err != nil {
		return 0, err
	}
	if _, err := s.ProcessConfigurationSignals(ctx); err != nil {
		return 0, err
	}
	if _, err := s.ProcessTopologySignals(ctx); err != nil {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73512012)`); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT incident_id FROM correlation_pending ORDER BY requested_at,incident_id LIMIT 25 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return 0, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err = s.projectIncidentCorrelation(ctx, tx, id); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM correlation_pending WHERE incident_id=$1`, id); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit(ctx)
}
