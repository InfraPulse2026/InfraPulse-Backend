package storage

import (
	"context"
	"encoding/json"
	"errors"
	cd "github.com/InfraPulse2026/InfraPulse-Backend/internal/configdrift"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/jackc/pgx/v5"
	"strconv"
	"time"
)

func queueConfigurationSignal(ctx context.Context, tx pgx.Tx, asset string) error {
	_, err := tx.Exec(ctx, `INSERT INTO configuration_signal_pending(asset_id) VALUES($1) ON CONFLICT(asset_id) DO UPDATE SET requested_at=now()`, asset)
	return err
}

type configurationSignalInput struct {
	evidence                                                                         health.ConfigurationEvidence
	revision                                                                         int64
	scope, lifecycle, resolution, template, targetTemplate, status, actualNormalizer string
	enabled                                                                          bool
	actualTarget                                                                     int64
	observed                                                                         time.Time
	comparison                                                                       []byte
}

const configurationSignalQuery = `SELECT a.id,a.revision,a.scope,a.lifecycle_status,a.resolution_status,b.asset_revision,b.revision,b.normalizer,b.read_template_id,t.revision,(t.definition->>'enabled')::boolean,t.definition->>'template_id',r.id,r.actual_snapshot_id,r.actual_target_revision,r.actual_observed_at,r.status,r.normalizer,r.comparison
FROM assets a JOIN configuration_baseline_state c ON c.asset_id=a.id
JOIN configuration_baseline_versions b ON b.asset_id=c.asset_id AND b.revision=c.revision
JOIN automation_targets t ON t.asset_id=a.id
JOIN LATERAL (SELECT * FROM configuration_drift_results d WHERE d.asset_id=a.id AND d.baseline_revision=c.revision ORDER BY d.actual_observed_at DESC,d.id DESC LIMIT 1) r ON true
WHERE a.id=ANY($1::text[])`

func readConfigurationSignal(row interface{ Scan(...any) error }) (configurationSignalInput, error) {
	var v configurationSignalInput
	err := row.Scan(&v.evidence.AssetID, &v.revision, &v.scope, &v.lifecycle, &v.resolution, &v.evidence.AssetRevision, &v.evidence.BaselineRevision, &v.evidence.Normalizer, &v.template, &v.evidence.TargetRevision, &v.enabled, &v.targetTemplate, &v.evidence.ResultID, &v.evidence.SnapshotID, &v.actualTarget, &v.observed, &v.status, &v.actualNormalizer, &v.comparison)
	if err != nil {
		return v, err
	}
	var comparison cd.Comparison
	if err = json.Unmarshal(v.comparison, &comparison); err != nil {
		return v, err
	}
	if comparison.Status != v.status {
		return v, errors.New("configuration comparison status mismatch")
	}
	v.evidence.TotalChanges = comparison.TotalChanges
	v.evidence.Truncated = comparison.Truncated
	return v, nil
}

func (v configurationSignalInput) contextID() string {
	return strconv.FormatInt(v.evidence.AssetRevision, 10) + ":" + strconv.FormatInt(v.evidence.BaselineRevision, 10) + ":" + strconv.FormatInt(v.evidence.TargetRevision, 10)
}
func (s *Store) configurationSignalMaxAge() time.Duration {
	if s.configurationDriftMaxAge > 0 {
		return s.configurationDriftMaxAge
	}
	return cd.DefaultMaxAge
}
func (v configurationSignalInput) valid(now time.Time, maxAge time.Duration) bool {
	return v.scope == "" && v.lifecycle == "observed" && v.resolution != "conflict" && v.revision == v.evidence.AssetRevision && v.evidence.Normalizer == cd.NormalizerVersion && v.actualNormalizer == v.evidence.Normalizer && v.enabled && v.targetTemplate == v.template && v.actualTarget == v.evidence.TargetRevision && !v.observed.Before(time.Unix(0, 0)) && !v.observed.After(now.Add(30*time.Second)) && now.Sub(v.observed) <= maxAge && (v.status == "compliant" || v.status == "drift" && v.evidence.TotalChanges > 0)
}

func (s *Store) ProcessConfigurationSignals(ctx context.Context) (int, error) {
	if !s.automationEnabled {
		return 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73512015)`); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT asset_id FROM configuration_signal_pending ORDER BY requested_at,asset_id LIMIT 25`)
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
	for _, asset := range ids {
		// Match producer lock order: asset before pending. Baseline/target updates
		// require this asset exclusively; ingestion can enqueue again after commit.
		var id string
		if err = tx.QueryRow(ctx, `SELECT id FROM assets WHERE id=$1 FOR SHARE`, asset).Scan(&id); err != nil {
			return 0, err
		}
		if err = tx.QueryRow(ctx, `SELECT asset_id FROM configuration_signal_pending WHERE asset_id=$1 FOR UPDATE`, asset).Scan(&id); err != nil {
			return 0, err
		}
		v, e := readConfigurationSignal(tx.QueryRow(ctx, configurationSignalQuery, []string{asset}))
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return 0, e
		}
		if e == nil && v.valid(time.Now().UTC(), s.configurationSignalMaxAge()) {
			if err = s.applyConfigurationSignal(ctx, tx, v); err != nil {
				return 0, err
			}
		}
		if _, err = tx.Exec(ctx, `DELETE FROM configuration_signal_pending WHERE asset_id=$1`, asset); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit(ctx)
}

func (s *Store) applyConfigurationSignal(ctx context.Context, tx pgx.Tx, v configurationSignalInput) error {
	var observed time.Time
	err := tx.QueryRow(ctx, `SELECT observed_at FROM configuration_signal_state WHERE asset_id=$1 AND context_id=$2`, v.evidence.AssetID, v.contextID()).Scan(&observed)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && !v.observed.After(observed) {
		return nil
	}
	status, severity, reason := "warning", "warning", "differs_from_approved_baseline"
	recovered := v.status == "compliant"
	if recovered {
		status = "ok"
		severity = "none"
		reason = "matches_same_approved_baseline"
	}
	a := health.Assessment{RuleID: "configuration.approved_drift", RuleVersion: "1", Status: status, Severity: severity, Reason: reason, ObservedAt: &v.observed, RecoveryEligible: recovered, Evidence: []health.Point{}, Configuration: &v.evidence}
	encoded, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if err = applyResourceIncident(ctx, tx, "", "", "ansible", "", "configuration", v.evidence.AssetID, v.contextID(), a, encoded); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO configuration_signal_state(asset_id,context_id,result_id,observed_at) VALUES($1,$2,$3,$4) ON CONFLICT(asset_id,context_id) DO UPDATE SET result_id=excluded.result_id,observed_at=excluded.observed_at`, v.evidence.AssetID, v.contextID(), v.evidence.ResultID, v.observed)
	return err
}

func (s *Store) configurationIncidentFreshness(ctx context.Context, q freshnessQueryer, items []health.Incident, now time.Time) error {
	ids := []string{}
	for _, v := range items {
		if v.ResourceKind == "configuration" && v.State != "resolved" {
			ids = append(ids, v.ResourceID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, configurationSignalQuery, ids)
	if err != nil {
		return err
	}
	states := map[string]configurationSignalInput{}
	for rows.Next() {
		v, e := readConfigurationSignal(rows)
		if e != nil {
			rows.Close()
			return e
		}
		states[v.evidence.AssetID] = v
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for i := range items {
		v := &items[i]
		if v.ResourceKind != "configuration" || v.State == "resolved" {
			continue
		}
		current, ok := states[v.ResourceID]
		v.EvidenceStatus = "missing"
		if ok {
			v.EvidenceStatus = "stale"
			if s.automationEnabled && v.Signal.Configuration != nil && current.evidence.ResultID == v.Signal.Configuration.ResultID && current.contextID() == v.ResourceContext && current.valid(now, s.configurationSignalMaxAge()) && now.Sub(v.LastObservedAt) <= s.configurationSignalMaxAge() {
				v.EvidenceStatus = "fresh"
			}
		}
	}
	return nil
}
