package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/jackc/pgx/v5"
)

func monitoringText(s string, max int) bool {
	if len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func validateExternalMonitoring(source datasources.Source, b *externalmonitor.Batch) bool {
	if b == nil {
		return true
	}
	if source.Type != "ZABBIX" || source.Configuration.Zabbix == nil || source.Configuration.Zabbix.Monitoring == nil || !source.Configuration.Zabbix.Monitoring.Enabled || len(b.Measurements) > 100 || len(b.Signals) > 1200 {
		return false
	}
	allowed := map[string]bool{}
	for _, id := range source.Configuration.Zabbix.HostIDs {
		allowed[id] = true
	}
	mappings := map[string]externalmonitor.Mapping{}
	for _, m := range source.Configuration.Zabbix.Monitoring.ItemMappings {
		mappings[m.ItemID] = m
	}
	seen := map[string]bool{}
	now := time.Now()
	for _, v := range b.Measurements {
		m, ok := mappings[v.ItemID]
		if !ok || seen[v.ItemID] || v.HostID != m.HostID || !allowed[v.HostID] || v.Metric != m.Metric || v.Unit != m.Unit || v.Multiplier != m.Multiplier || v.MaxAgeSeconds != m.MaxAgeSeconds || !monitoringText(v.Name, 256) || !monitoringText(v.SourceUnit, 32) || v.Nanoseconds < 0 || v.Nanoseconds > 999999999 {
			return false
		}
		seen[v.ItemID] = true
		switch v.Quality {
		case "fresh", "stale", "disabled", "unsupported", "non_numeric", "unit_mismatch", "no_data", "clock_skew", "invalid_value", "unavailable":
		default:
			return false
		}
		if v.RawValue != "" {
			if _, ok := externalmonitor.Number(v.RawValue); !ok {
				return false
			}
		}
		if v.Value != nil {
			if _, ok := externalmonitor.Number(*v.Value); !ok || v.ObservedAt == nil {
				return false
			}
		}
		if v.ObservedAt != nil && (v.ObservedAt.Year() < 1970 || v.ObservedAt.Year() > 9999 || v.ObservedAt.After(now.Add(30*time.Second))) {
			return false
		}
	}
	seen = map[string]bool{}
	for _, v := range b.Signals {
		if !externalmonitor.ValidID(v.EventID) || !externalmonitor.ValidID(v.TriggerID) || seen[v.EventID] || len(v.HostIDs) == 0 || len(v.HostIDs) > 200 || v.Severity < 0 || v.Severity > 5 || v.Nanoseconds < 0 || v.Nanoseconds > 999999999 || !monitoringText(v.Name, 1024) || !monitoringText(v.TriggerName, 1024) || v.ObservedAt.Year() < 1970 || v.ObservedAt.Year() > 9999 || v.ObservedAt.After(now.Add(30*time.Second)) {
			return false
		}
		seen[v.EventID] = true
		seenHosts := map[string]bool{}
		for _, id := range v.HostIDs {
			if !allowed[id] || seenHosts[id] {
				return false
			}
			seenHosts[id] = true
		}
		if v.State != "active" && v.State != "resolved" || v.State == "active" && (v.RecoveredAt != nil || v.RecoveryEventID != "") || v.State == "resolved" && (!externalmonitor.ValidID(v.RecoveryEventID) || v.RecoveredAt == nil || v.RecoveredAt.Before(v.ObservedAt) || v.RecoveredAt.After(now.Add(30*time.Second))) {
			return false
		}
		if v.MappingStatus != "host_filter" && v.MappingStatus != "trigger_unavailable" {
			return false
		}
	}
	raw, err := json.Marshal(b)
	return err == nil && len(raw) <= 4<<20
}
func stableJSON(value any) ([]byte, string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:]), nil
}

func (s *Store) pendingExternalSignals(ctx context.Context, tx pgx.Tx, source datasources.Source, rawCursor []byte) ([]externalmonitor.Pending, error) {
	if source.Type != "ZABBIX" || source.Configuration.Zabbix == nil || source.Configuration.Zabbix.Monitoring == nil || !source.Configuration.Zabbix.Monitoring.Enabled {
		return nil, nil
	}
	var c struct {
		After string `json:"reconcile_after"`
	}
	_ = json.Unmarshal(rawCursor, &c)
	if c.After == "" || !externalmonitor.ValidID(c.After) {
		c.After = "0"
	}
	rows, err := tx.Query(ctx, `SELECT s.event_id,s.trigger_id,array_agg(h.host_id ORDER BY h.host_id) FROM external_signals s JOIN external_signal_hosts h USING(source_id,event_id) WHERE s.source_id=$1 AND s.state='active' AND s.event_id::numeric>$2::numeric AND h.host_id=ANY($3::text[]) GROUP BY s.event_id,s.trigger_id ORDER BY s.event_id::numeric LIMIT 100`, source.ID, c.After, source.Configuration.Zabbix.HostIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []externalmonitor.Pending{}
	for rows.Next() {
		var p externalmonitor.Pending
		if err = rows.Scan(&p.EventID, &p.TriggerID, &p.HostIDs); err != nil {
			return nil, err
		}
		items = append(items, p)
	}
	return items, rows.Err()
}

func (s *Store) projectExternalMonitoring(ctx context.Context, tx pgx.Tx, source datasources.Source, b *externalmonitor.Batch) error {
	if b == nil {
		return nil
	}
	for _, v := range b.Measurements {
		raw, hash, err := stableJSON(v)
		if err != nil {
			return err
		}
		var sampleID int64
		// Same latest value/time/quality is one snapshot, even across polling retries.
		err = tx.QueryRow(ctx, `INSERT INTO external_metric_samples(source_id,item_id,host_id,source_revision,fingerprint,payload) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(source_id,item_id,fingerprint) DO UPDATE SET fingerprint=EXCLUDED.fingerprint RETURNING id`, source.ID, v.ItemID, v.HostID, source.Revision, hash, raw).Scan(&sampleID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO external_metrics(source_id,item_id,host_id,source_revision,latest_sample_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT(source_id,item_id) DO UPDATE SET host_id=EXCLUDED.host_id,source_revision=EXCLUDED.source_revision,latest_sample_id=EXCLUDED.latest_sample_id,last_seen=now()`, source.ID, v.ItemID, v.HostID, source.Revision, sampleID)
		if err != nil {
			return err
		}
	}
	for _, v := range b.Signals {
		var oldState, oldHash, incidentID, oldTrigger string
		err := tx.QueryRow(ctx, `SELECT state,fingerprint,COALESCE(incident_id,''),trigger_id FROM external_signals WHERE source_id=$1 AND event_id=$2 FOR UPDATE`, source.ID, v.EventID).Scan(&oldState, &oldHash, &incidentID, &oldTrigger)
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if exists && oldTrigger != v.TriggerID {
			return externalmonitor.ErrInvalid
		}
		// A stale active response cannot reopen an explicitly recovered event.
		if oldState == "resolved" && v.State == "active" {
			continue
		}
		// Accumulate source host memberships without guessing global Asset IDs.
		hosts := map[string]bool{}
		for _, id := range v.HostIDs {
			hosts[id] = true
		}
		if exists {
			rows, err := tx.Query(ctx, `SELECT host_id FROM external_signal_hosts WHERE source_id=$1 AND event_id=$2`, source.ID, v.EventID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id string
				if err = rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				hosts[id] = true
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		v.HostIDs = []string{}
		for id := range hosts {
			v.HostIDs = append(v.HostIDs, id)
		}
		sortIDs(v.HostIDs)
		raw, hash, err := stableJSON(v)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO external_signals(source_id,event_id,trigger_id,source_revision,state,payload,fingerprint) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(source_id,event_id) DO UPDATE SET source_revision=EXCLUDED.source_revision,state=EXCLUDED.state,payload=EXCLUDED.payload,fingerprint=EXCLUDED.fingerprint,last_seen=now()`, source.ID, v.EventID, v.TriggerID, source.Revision, v.State, raw, hash)
		if err != nil {
			return err
		}
		for _, id := range v.HostIDs {
			if _, err = tx.Exec(ctx, `INSERT INTO external_signal_hosts(source_id,event_id,host_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, source.ID, v.EventID, id); err != nil {
				return err
			}
		}
		if !exists || hash != oldHash {
			if _, err = tx.Exec(ctx, `INSERT INTO external_signal_versions(source_id,event_id,source_revision,fingerprint,payload) VALUES($1,$2,$3,$4,$5)`, source.ID, v.EventID, source.Revision, hash, raw); err != nil {
				return err
			}
		}
		if err = s.applyExternalIncident(ctx, tx, source, v, incidentID, !exists || hash != oldHash); err != nil {
			return err
		}
	}
	return nil
}
func sortIDs(ids []string) {
	sort.Slice(ids, func(i, j int) bool {
		a, _ := strconv.ParseUint(ids[i], 10, 64)
		b, _ := strconv.ParseUint(ids[j], 10, 64)
		return a < b
	})
}

func (s *Store) applyExternalIncident(ctx context.Context, tx pgx.Tx, source datasources.Source, v externalmonitor.Signal, id string, changed bool) error {
	severity := "warning"
	if v.Severity >= 4 {
		severity = "critical"
	}
	status := severity
	recovery := v.State == "resolved"
	if recovery {
		status = "ok"
	}
	now := time.Now().UTC()
	assessment := health.Assessment{RuleID: "zabbix.problem", RuleVersion: "external-v1", Status: status, Severity: severity, Reason: "external_problem_reported", ObservedAt: &now, RecoveryEligible: recovery, Evidence: []health.Point{}, External: &v}
	if recovery {
		assessment.Reason = "external_recovery_reported"
	}
	assessment.SourceRevision = source.Revision
	raw, err := json.Marshal(assessment)
	if err != nil {
		return err
	}
	state := "open"
	reason := "external_problem_reported"
	kind := "opened"
	if id == "" {
		id, err = access.NewID()
		if err != nil {
			return err
		}
		id = "inc_ext_" + id
		_, fingerprint, err := stableJSON([]string{"external", source.ID, v.EventID})
		if err != nil {
			return err
		}
		if recovery {
			state = "resolved"
			reason = "external_historical_recovery"
			kind = "historical"
		}
		_, err = tx.Exec(ctx, `INSERT INTO incidents(id,fingerprint,agent_id,device_id,source,scope,rule_id,occurrence,state,severity,opened_at,last_observed_at,resolved_at,signal,resource_kind,resource_id,resource_context,source_instance_id,external_event_id) VALUES($1,$2,NULL,NULL,'zabbix','','zabbix.problem',1,$3,$4,$5,now(),$6,$7,'external',$8,$9,$9,$10)`, id, fingerprint, state, severity, v.ObservedAt, v.RecoveredAt, raw, v.TriggerID, source.ID, v.EventID)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE external_signals SET incident_id=$3 WHERE source_id=$1 AND event_id=$2`, source.ID, v.EventID, id); err != nil {
			return err
		}
	} else {
		var previousSeverity string
		if err = tx.QueryRow(ctx, `SELECT state,severity FROM incidents WHERE id=$1 FOR UPDATE`, id).Scan(&state, &previousSeverity); err != nil {
			return err
		}
		reason = ""
		kind = "updated"
		if recovery && state != "resolved" {
			state = "resolved"
			reason = "external_recovery_reported"
			kind = "recovered"
		} else if severity != previousSeverity {
			reason = "severity_changed"
		}
		if _, err = tx.Exec(ctx, `UPDATE incidents SET state=$2,severity=$3,last_observed_at=now(),updated_at=now(),resolved_at=COALESCE($4,resolved_at),signal=$5 WHERE id=$1`, id, state, severity, v.RecoveredAt, raw); err != nil {
			return err
		}
	}
	if reason != "" {
		if _, err = tx.Exec(ctx, `INSERT INTO incident_transitions(incident_id,state,reason) VALUES($1,$2,$3)`, id, state, reason); err != nil {
			return err
		}
	}
	if changed {
		_, err = tx.Exec(ctx, `INSERT INTO incident_evidence(incident_id,kind,observed_at,assessment) VALUES($1,$2,$3,$4)`, id, kind, v.ObservedAt, raw)
	}
	return err
}
