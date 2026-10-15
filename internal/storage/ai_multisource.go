package storage

import (
	"context"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/analysisai"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/jackc/pgx/v5"
	"strconv"
	"time"
)

func (s *Store) aiLegacyPayload(ctx context.Context, incident string) (analysisai.Payload, error) {
	diagnostic, err := s.IncidentContext(ctx, incident, 300, 8)
	if err != nil {
		return analysisai.Payload{}, err
	}
	counts := map[string]int64{}
	from, to := diagnostic.Seed.LastObservedAt.Add(-5*time.Minute), diagnostic.Seed.LastObservedAt.Add(5*time.Minute)
	demo := diagnostic.Seed.Source == "simulated" || diagnostic.Seed.Scope != ""
	rows, err := s.pool.Query(ctx, `SELECT CASE WHEN severity BETWEEN 0 AND 7 THEN severity::text ELSE 'unknown' END,count(*) FROM log_records WHERE agent_id=$1 AND ($2='' OR device_id=$2) AND event_at BETWEEN $3 AND $4 AND ((source='simulated' OR record_scope<>'')=$5) AND (NOT $5 OR record_scope=$6) GROUP BY 1`, diagnostic.Seed.AgentID, diagnostic.Seed.DeviceID, from, to, demo, diagnostic.Seed.Scope)
	if err != nil {
		return analysisai.Payload{}, err
	}
	names := []string{"emergency", "alert", "critical", "error", "warning", "notice", "info", "debug"}
	for rows.Next() {
		var severity string
		var count int64
		if err = rows.Scan(&severity, &count); err != nil {
			rows.Close()
			return analysisai.Payload{}, err
		}
		if index, e := strconv.Atoi(severity); e == nil && index >= 0 && index < 8 {
			severity = names[index]
		}
		counts[severity] = count
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return analysisai.Payload{}, err
	}
	var oom, disk, ioError, authFail, linkDown, sipTimeout int64
	err = s.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE lower(message) ~ '(out of memory|oom-killer|oom kill)'),count(*) FILTER(WHERE lower(message) LIKE '%no space left on device%'),count(*) FILTER(WHERE lower(message) LIKE '%i/o error%'),count(*) FILTER(WHERE lower(message) ~ '(authentication failure|failed password)'),count(*) FILTER(WHERE lower(message) LIKE '%link down%'),count(*) FILTER(WHERE lower(message) LIKE '%sip%' AND lower(message) LIKE '%timeout%') FROM log_records WHERE agent_id=$1 AND ($2='' OR device_id=$2) AND event_at BETWEEN $3 AND $4 AND ((source='simulated' OR record_scope<>'')=$5) AND (NOT $5 OR record_scope=$6)`, diagnostic.Seed.AgentID, diagnostic.Seed.DeviceID, from, to, demo, diagnostic.Seed.Scope).Scan(&oom, &disk, &ioError, &authFail, &linkDown, &sipTimeout)
	if err != nil {
		return analysisai.Payload{}, err
	}
	for tag, count := range map[string]int64{"out_of_memory": oom, "disk_full": disk, "io_error": ioError, "authentication_failure": authFail, "link_down": linkDown, "sip_timeout": sipTimeout} {
		counts["event:"+tag] = count
	}
	payload, err := analysisai.Export(diagnostic, counts)
	if err != nil {
		return analysisai.Payload{}, err
	}

	return payload, nil
}

func (s *Store) aiMultiSourcePayload(ctx context.Context, seed string) (analysisai.Payload, []analysisai.EvidenceReference, *string, int64, error) {
	var caseID string
	err := s.pool.QueryRow(ctx, `SELECT m.correlated_incident_id FROM correlated_incident_signals m JOIN normalized_signals n ON n.id=m.signal_id WHERE n.incident_id=$1`, seed).Scan(&caseID)
	if errors.Is(err, pgx.ErrNoRows) {
		p, e := s.aiLegacyPayload(ctx, seed)
		return p, nil, nil, 0, e
	}
	if err != nil {
		return analysisai.Payload{}, nil, nil, 0, err
	}
	card, err := s.CorrelatedIncident(ctx, caseID)
	if err != nil {
		return analysisai.Payload{}, nil, nil, 0, err
	}
	ids := []string{}
	for _, m := range card.Signals {
		ids = append(ids, m.Signal.IncidentID)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return analysisai.Payload{}, nil, nil, 0, err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id=ANY($1::text[])`, ids)
	if err != nil {
		return analysisai.Payload{}, nil, nil, 0, err
	}
	items := []health.Incident{}
	for rows.Next() {
		v, e := readIncident(rows)
		if e != nil {
			rows.Close()
			return analysisai.Payload{}, nil, nil, 0, e
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return analysisai.Payload{}, nil, nil, 0, err
	}
	if err = s.incidentFreshnessTx(ctx, tx, items, time.Now().UTC()); err != nil {
		return analysisai.Payload{}, nil, nil, 0, err
	}
	current := map[string]health.Incident{}
	for _, v := range items {
		current[v.ID] = v
	}
	payload, refs, err := analysisai.ExportCorrelated(card, seed, current)
	if err != nil {
		return payload, nil, nil, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return payload, nil, nil, 0, err
	}
	return payload, refs, &caseID, card.Incident.Revision, nil
}
