package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assethardware"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/jackc/pgx/v5"
)

const hardwareSignalJoins = ` FROM asset_hardware_samples h JOIN asset_source_bindings b ON b.id=h.binding_id JOIN data_sources s ON s.id=h.source_instance_id JOIN assets live ON live.id=b.asset_id LEFT JOIN agents a ON a.agent_id=b.legacy_agent_id LEFT JOIN monitoring_policies p ON p.agent_id=b.legacy_agent_id AND p.device_id=b.external_id `

const hardwareCurrentSource = ` AND (b.inventory->'observation'->'source' IS NULL OR (
COALESCE(h.payload->'source_ref'->>'type','')=COALESCE(b.inventory->'observation'->'source'->>'type','') AND
COALESCE(h.payload->'source_ref'->>'target_id','')=COALESCE(b.inventory->'observation'->'source'->>'target_id','') AND
COALESCE(h.payload->'source_ref'->>'protocol','')=COALESCE(b.inventory->'observation'->'source'->>'protocol','') AND
COALESCE(h.payload->'source_ref'->>'instance_id','')=COALESCE(b.inventory->'observation'->'source'->>'instance_id',''))) `

// Each sample is durably queued with its envelope. Process bounded pages outside
// the gRPC ACK transaction. A failed page leaves all samples available for retry.
func (s *Store) ProcessHardwareSignals(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73512013)`); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT sample_id FROM hardware_signal_pending ORDER BY sample_id LIMIT 25 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return 0, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
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
		if err = s.applyHardwareSample(ctx, tx, id); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM hardware_signal_pending WHERE sample_id=$1`, id); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit(ctx)
}

func (s *Store) applyHardwareSample(ctx context.Context, tx pgx.Tx, id int64) error {
	now := time.Now().UTC()
	rows, err := tx.Query(ctx, `SELECT `+strings.ReplaceAll(hardwareReadColumns, "$6", "$2")+hardwareSignalJoins+`WHERE h.id=$1 AND live.lifecycle_status='observed' AND live.resolution_status<>'conflict'`+hardwareCurrentSource, id, s.timeout.Seconds())
	if err != nil {
		return err
	}
	items, _, err := readHardwareRows(rows, "", nil, now, 300, 1, false)
	if err != nil || len(items) == 0 {
		return err
	}
	i := items[0]
	if i.ObservedAt.After(now.Add(30 * time.Second)) {
		return nil
	}
	var agent, device, session, lastSession string
	if err = tx.QueryRow(ctx, `SELECT b.legacy_agent_id,b.legacy_device_id,a.session_id FROM asset_source_bindings b JOIN agents a ON a.agent_id=b.legacy_agent_id WHERE b.id=$1`, i.BindingID).Scan(&agent, &device, &session); err != nil {
		return err
	}
	var last time.Time
	var sourceAt *time.Time
	var count int
	var evidenceRaw []byte
	err = tx.QueryRow(ctx, `SELECT last_observed_at,last_source_at,recovery_count,recovery_evidence,last_session_id FROM hardware_signal_state WHERE observation_key=$1 FOR UPDATE`, i.ObservationKey).Scan(&last, &sourceAt, &count, &evidenceRaw, &lastSession)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !last.IsZero() && !i.ObservedAt.After(last) {
		return nil
	}
	evidence := []health.ResourcePoint{}
	if len(evidenceRaw) > 0 {
		if err = json.Unmarshal(evidenceRaw, &evidence); err != nil {
			return err
		}
	}
	status := assethardware.Status(i.Observation)
	if i.Freshness != "fresh" {
		status = "unknown"
	}
	// Repeated source timestamps are one sensor reading, even in new envelopes.
	advance := i.SourceTimestamp == nil || sourceAt == nil || i.SourceTimestamp.After(*sourceAt)
	if status != "ok" || !advance || session != lastSession || !last.IsZero() && i.ObservedAt.Sub(last) > time.Duration(i.FreshnessMaxAgeSeconds)*time.Second {
		count = 0
		evidence = []health.ResourcePoint{}
	}
	if status == "ok" && advance {
		if count < 3 {
			count++
		}
		evidence = append(evidence, health.ResourcePoint{RecordID: i.ID, RecordKind: "hardware", ObservedAt: i.ObservedAt, State: "ok"})
		if len(evidence) > 3 {
			evidence = evidence[len(evidence)-3:]
		}
	}
	encodedEvidence, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	if i.SourceTimestamp != nil && (sourceAt == nil || i.SourceTimestamp.After(*sourceAt)) && !i.SourceTimestamp.After(now.Add(30*time.Second)) {
		sourceAt = i.SourceTimestamp
	}
	if _, err = tx.Exec(ctx, `INSERT INTO hardware_signal_state(observation_key,last_observed_at,last_source_at,recovery_count,recovery_evidence,last_session_id) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(observation_key) DO UPDATE SET last_observed_at=EXCLUDED.last_observed_at,last_source_at=EXCLUDED.last_source_at,recovery_count=EXCLUDED.recovery_count,recovery_evidence=EXCLUDED.recovery_evidence,last_session_id=EXCLUDED.last_session_id,updated_at=now()`, i.ObservationKey, i.ObservedAt, sourceAt, count, encodedEvidence, session); err != nil {
		return err
	}
	if status == "unknown" {
		return nil
	}
	severity := "none"
	reason := "vendor_health_ok"
	if status == "warning" || status == "critical" {
		severity = status
		reason = "vendor_health_problem"
		evidence = []health.ResourcePoint{{RecordID: i.ID, RecordKind: "hardware", ObservedAt: i.ObservedAt, State: status}}
	}
	a := health.Assessment{RuleID: "hardware.vendor_health", RuleVersion: "1", Status: status, Severity: severity, Reason: reason, ObservedAt: &i.ObservedAt, RecoveryEligible: status == "ok" && count == 3, ResourceEvidence: evidence, Evidence: []health.Point{}, Hardware: &health.HardwareEvidence{SampleID: i.ID, ObservationKey: i.ObservationKey, BindingID: i.BindingID, ResourceID: i.ResourceID, SubjectID: i.SubjectID, Health: assethardware.CanonicalHealth(i.Health), HealthRollup: assethardware.CanonicalHealth(i.HealthRollup), ClockQuality: i.ClockQuality, Freshness: i.Freshness, RecoveryCount: count}}
	encoded, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return applyResourceIncident(ctx, tx, agent, device, i.SourceType, i.Scope, "hardware", i.ObservationKey, i.BindingID, a, encoded)
}

func (s *Store) hardwareIncidentFreshness(ctx context.Context, query freshnessQueryer, incidents []health.Incident, now time.Time) error {
	keys := []string{}
	for _, v := range incidents {
		if v.ResourceKind == "hardware" && v.State != "resolved" {
			keys = append(keys, v.ResourceID)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	rows, err := query.Query(ctx, `SELECT `+strings.ReplaceAll(hardwareReadColumns, "$6", "$2")+hardwareSignalJoins+`JOIN asset_hardware_latest l ON l.sample_id=h.id WHERE l.observation_key=ANY($1::text[]) AND live.lifecycle_status='observed' AND live.resolution_status<>'conflict'`+hardwareCurrentSource, keys, s.timeout.Seconds())
	if err != nil {
		return err
	}
	items, _, err := readHardwareRows(rows, "", nil, now, 300, 200, false)
	if err != nil {
		return err
	}
	byKey := map[string]assethardware.Item{}
	for _, v := range items {
		byKey[v.ObservationKey] = v
	}
	for idx := range incidents {
		v := &incidents[idx]
		if v.ResourceKind != "hardware" || v.State == "resolved" {
			continue
		}
		v.EvidenceStatus = "missing"
		if i, ok := byKey[v.ResourceID]; ok && i.BindingID == v.ResourceContext {
			v.EvidenceStatus = i.Freshness
			if v.EvidenceStatus == "fresh" && assethardware.Status(i.Observation) == "unknown" {
				v.EvidenceStatus = "unknown"
			}
		}
	}
	return nil
}
