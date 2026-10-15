package storage

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	cd "github.com/InfraPulse2026/InfraPulse-Backend/internal/configdrift"
	"github.com/jackc/pgx/v5"
	"math"
	"time"
)

const baselineColumns = `v.asset_id,v.revision,v.asset_revision,v.snapshot_id,v.read_template_id,v.normalizer,v.raw_digest,v.normalized_digest,v.actor_id,v.reason,v.created_at`

func readBaseline(row interface{ Scan(...any) error }) (cd.Baseline, error) {
	var v cd.Baseline
	err := row.Scan(&v.AssetID, &v.Revision, &v.AssetRevision, &v.SnapshotID, &v.ReadTemplateID, &v.Normalizer, &v.RawDigest, &v.NormalizedDigest, &v.ActorID, &v.Reason, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return v, err
}
func (s *Store) ConfigurationBaseline(ctx context.Context, asset string) (cd.Baseline, error) {
	return readBaseline(s.pool.QueryRow(ctx, `SELECT `+baselineColumns+` FROM configuration_baseline_versions v JOIN configuration_baseline_state c ON c.asset_id=v.asset_id AND c.revision=v.revision WHERE v.asset_id=$1`, asset))
}
func (s *Store) ConfigurationBaselineHistory(ctx context.Context, asset string, limit, offset int) ([]cd.Baseline, error) {
	if !au.Identifier(asset) || limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, cd.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT `+baselineColumns+` FROM configuration_baseline_versions v WHERE asset_id=$1 ORDER BY revision DESC LIMIT $2 OFFSET $3`, asset, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []cd.Baseline{}
	for rows.Next() {
		v, e := readBaseline(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type driftSnapshot struct {
	au.Snapshot
	TargetRevision int64
	AssetRevision  int64
	Payload        au.Result
}

func (s *Store) driftSnapshotTx(ctx context.Context, tx pgx.Tx, asset, id string) (driftSnapshot, error) {
	var v driftSnapshot
	var cipher []byte
	err := tx.QueryRow(ctx, `SELECT snap.id,snap.asset_id,snap.job_id,snap.template_id,snap.digest,snap.bytes,snap.observed_at,j.target_revision,(j.target->>'asset_revision')::bigint,snap.ciphertext FROM automation_snapshots snap JOIN automation_jobs j ON j.id=snap.job_id WHERE snap.asset_id=$1 AND snap.id=$2 AND j.state='succeeded'`, asset, id).Scan(&v.ID, &v.AssetID, &v.JobID, &v.TemplateID, &v.Digest, &v.Bytes, &v.ObservedAt, &v.TargetRevision, &v.AssetRevision, &cipher)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	plain, err := s.automationVault.Open("automation-snapshot:"+id, cipher)
	if err != nil {
		return v, cd.ErrConflict
	}
	defer clear(plain)
	if json.Unmarshal(plain, &v.Payload) != nil || v.Payload.Validate() != nil || au.Digest(v.Payload.Configuration) != v.Digest {
		return v, cd.ErrConflict
	}
	return v, nil
}
func (s *Store) PutConfigurationBaseline(ctx context.Context, asset string, in cd.BaselineInput, key string) (cd.Baseline, bool, error) {
	var out cd.Baseline
	if !s.automationEnabled {
		return out, false, au.ErrUnavailable
	}
	if !au.Identifier(asset) || in.Validate() != nil || commands.ValidateKey(key) != nil || in.ExpectedRevision == math.MaxInt64 {
		return out, false, cd.ErrInvalid
	}
	actor, ok := access.Current(ctx)
	if !ok {
		return out, false, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(context.Background())
	if _, err = automationActorTx(ctx, tx, actor, true); err != nil {
		return out, false, err
	}
	raw, _ := json.Marshal(struct {
		Asset string
		Input cd.BaselineInput
	}{asset, in})
	hash := au.Digest(string(raw))
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194005)`); err != nil {
		return out, false, err
	}
	var oldHash string
	err = tx.QueryRow(ctx, `SELECT request_hash FROM configuration_baseline_versions WHERE actor_id=$1 AND idempotency_key=$2`, actor.User.ID, key).Scan(&oldHash)
	if err == nil {
		if oldHash != hash {
			return out, false, cd.ErrConflict
		}
		out, err = readBaseline(tx.QueryRow(ctx, `SELECT `+baselineColumns+` FROM configuration_baseline_versions v WHERE actor_id=$1 AND idempotency_key=$2`, actor.User.ID, key))
		if err != nil {
			return out, false, err
		}
		return out, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, false, err
	}
	var assetRevision int64
	var lifecycle, scope string
	if err = tx.QueryRow(ctx, `SELECT revision,lifecycle_status,scope FROM assets WHERE id=$1 FOR UPDATE`, asset).Scan(&assetRevision, &lifecycle, &scope); errors.Is(err, pgx.ErrNoRows) {
		return out, false, ErrNotFound
	} else if err != nil {
		return out, false, err
	}
	if assetRevision != in.AssetRevision || scope != "" || lifecycle != "observed" {
		return out, false, cd.ErrConflict
	}
	var current int64
	err = tx.QueryRow(ctx, `SELECT revision FROM configuration_baseline_state WHERE asset_id=$1 FOR UPDATE`, asset).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, false, err
	}
	if current != in.ExpectedRevision {
		return out, false, cd.ErrConflict
	}
	snapshot, err := s.driftSnapshotTx(ctx, tx, asset, in.SnapshotID)
	if err != nil {
		return out, false, err
	}
	var template string
	if err = tx.QueryRow(ctx, `SELECT definition->>'template_id' FROM automation_targets WHERE asset_id=$1`, asset).Scan(&template); err != nil {
		return out, false, cd.ErrConflict
	}
	if snapshot.TemplateID != template || snapshot.AssetRevision != assetRevision {
		return out, false, cd.ErrConflict
	}
	normalized, err := cd.Digest(template, snapshot.Payload.Configuration)
	if err != nil {
		return out, false, cd.ErrInvalid
	}
	out, err = readBaseline(tx.QueryRow(ctx, `INSERT INTO configuration_baseline_versions AS v(asset_id,revision,asset_revision,snapshot_id,read_template_id,normalizer,raw_digest,normalized_digest,actor_id,reason,idempotency_key,request_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING `+baselineColumns, asset, current+1, assetRevision, snapshot.ID, template, cd.NormalizerVersion, snapshot.Digest, normalized, actor.User.ID, in.Reason, key, hash))
	if err != nil {
		return out, false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO configuration_baseline_state(asset_id,revision) VALUES($1,$2) ON CONFLICT(asset_id) DO UPDATE SET revision=excluded.revision`, asset, out.Revision); err != nil {
		return out, false, err
	}
	if err = automationAudit(ctx, tx, actor.User.ID, "configuration.baseline.approve", "success", asset, access.CurrentMeta(ctx)); err != nil {
		return out, false, err
	}
	var latest string
	err = tx.QueryRow(ctx, `SELECT snap.id FROM automation_snapshots snap JOIN automation_jobs j ON j.id=snap.job_id WHERE snap.asset_id=$1 AND snap.template_id=$2 AND j.state='succeeded' AND (j.target->>'asset_revision')::bigint=$3 ORDER BY snap.observed_at DESC,snap.id DESC LIMIT 1`, asset, template, assetRevision).Scan(&latest)
	if err == nil {
		actual, e := s.driftSnapshotTx(ctx, tx, asset, latest)
		if e != nil {
			return out, false, e
		}
		if _, e = s.compareConfigurationTx(ctx, tx, out, actual); e != nil {
			return out, false, e
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return out, false, err
	}
	return out, false, tx.Commit(ctx)
}

const driftRecordColumns = `id,asset_id,baseline_revision,actual_snapshot_id,actual_target_revision,normalizer,comparison,related_services,actual_observed_at,compared_at`

func readDriftRecord(row interface{ Scan(...any) error }) (cd.Record, error) {
	var v cd.Record
	var comparison, services []byte
	err := row.Scan(&v.ID, &v.AssetID, &v.BaselineRevision, &v.ActualSnapshotID, &v.ActualTargetRevision, &v.Normalizer, &comparison, &services, &v.ActualObservedAt, &v.ComparedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal(comparison, &v.Comparison); err != nil {
		return v, err
	}
	err = json.Unmarshal(services, &v.RelatedServices)
	return v, err
}
func (s *Store) compareConfigurationTx(ctx context.Context, tx pgx.Tx, baseline cd.Baseline, actual driftSnapshot) (cd.Record, error) {
	out, err := readDriftRecord(tx.QueryRow(ctx, `SELECT `+driftRecordColumns+` FROM configuration_drift_results WHERE asset_id=$1 AND baseline_revision=$2 AND actual_snapshot_id=$3`, baseline.AssetID, baseline.Revision, actual.ID))
	if err == nil {
		return out, queueConfigurationSignal(ctx, tx, baseline.AssetID)
	}
	if !errors.Is(err, ErrNotFound) {
		return out, err
	}
	comparison := cd.Comparison{Status: "unknown", ErrorCode: "baseline_or_source_mismatch", Changes: []cd.Change{}}
	if baseline.Normalizer == cd.NormalizerVersion && baseline.AssetRevision == actual.AssetRevision && baseline.ReadTemplateID == actual.TemplateID {
		expected, e := s.driftSnapshotTx(ctx, tx, baseline.AssetID, baseline.SnapshotID)
		if e != nil {
			comparison.ErrorCode = "expected_snapshot_unreadable"
		} else {
			comparison = cd.Compare(baseline.ReadTemplateID, expected.Payload.Configuration, actual.Payload.Configuration)
			if comparison.ExpectedDigest != "" && comparison.ExpectedDigest != baseline.NormalizedDigest {
				comparison = cd.Comparison{Status: "unknown", ErrorCode: "baseline_digest_mismatch", Changes: []cd.Change{}}
			}
		}
	}
	// Association establishes related VoIP context, not proof that this port/VLAN
	// carries that service. No address, target URI or arbitrary service name leaks.
	services := []cd.Service{}
	if comparison.Status == "drift" {
		rows, e := tx.Query(ctx, `SELECT v.id,v.service_kind FROM asset_voip_associations v JOIN asset_source_bindings b ON b.id=v.binding_id WHERE b.asset_id=$1 AND v.enabled AND v.scope='' ORDER BY v.id LIMIT 50`, baseline.AssetID)
		if e != nil {
			return out, e
		}
		for rows.Next() {
			var service cd.Service
			service.Relation = "reviewed_asset_association_not_proven_port_impact"
			if e = rows.Scan(&service.AssociationID, &service.Kind); e != nil {
				rows.Close()
				return out, e
			}
			services = append(services, service)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return out, e
		}
	}
	raw, _ := json.Marshal(comparison)
	related, _ := json.Marshal(services)
	// Idempotency also handles concurrent explicit comparisons safely.
	out, err = readDriftRecord(tx.QueryRow(ctx, `INSERT INTO configuration_drift_results(asset_id,baseline_revision,actual_snapshot_id,actual_target_revision,normalizer,status,error_code,comparison,related_services,actual_observed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(asset_id,baseline_revision,actual_snapshot_id) DO UPDATE SET actual_snapshot_id=excluded.actual_snapshot_id RETURNING `+driftRecordColumns, baseline.AssetID, baseline.Revision, actual.ID, actual.TargetRevision, cd.NormalizerVersion, comparison.Status, comparison.ErrorCode, raw, related, actual.ObservedAt))
	if err == nil {
		err = queueConfigurationSignal(ctx, tx, baseline.AssetID)
	}
	return out, err
}

// Called inside the successful B10 job transaction after its state and snapshot
// are persisted. Drift is projected atomically; a retry cannot duplicate it.
func (s *Store) projectConfigurationDrift(ctx context.Context, tx pgx.Tx, j au.Job, snapshotID string) error {
	baseline, err := readBaseline(tx.QueryRow(ctx, `SELECT `+baselineColumns+` FROM configuration_baseline_versions v JOIN configuration_baseline_state c ON c.asset_id=v.asset_id AND c.revision=v.revision WHERE v.asset_id=$1`, j.AssetID))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	actual, err := s.driftSnapshotTx(ctx, tx, j.AssetID, snapshotID)
	if err != nil {
		return err
	}
	_, err = s.compareConfigurationTx(ctx, tx, baseline, actual)
	return err
}
func (s *Store) CompareConfiguration(ctx context.Context, asset string, in cd.CompareInput) (cd.Record, error) {
	var out cd.Record
	if !au.Identifier(asset) || in.Validate() != nil {
		return out, cd.ErrInvalid
	}
	actor, ok := access.Current(ctx)
	if !ok {
		return out, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	if _, err = automationActorTx(ctx, tx, actor, false); err != nil {
		return out, err
	}
	var assetRevision int64
	var scope, lifecycle string
	if err = tx.QueryRow(ctx, `SELECT revision,scope,lifecycle_status FROM assets WHERE id=$1 FOR SHARE`, asset).Scan(&assetRevision, &scope, &lifecycle); errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	} else if err != nil {
		return out, err
	}
	baseline, err := readBaseline(tx.QueryRow(ctx, `SELECT `+baselineColumns+` FROM configuration_baseline_versions v JOIN configuration_baseline_state c ON c.asset_id=v.asset_id AND c.revision=v.revision WHERE v.asset_id=$1 FOR SHARE OF c`, asset))
	if err != nil {
		return out, err
	}
	if baseline.Revision != in.BaselineRevision || baseline.AssetRevision != assetRevision || lifecycle != "observed" || scope != "" {
		return out, cd.ErrConflict
	}
	actual, err := s.driftSnapshotTx(ctx, tx, asset, in.SnapshotID)
	if err != nil {
		return out, err
	}
	if actual.TemplateID != baseline.ReadTemplateID || actual.AssetRevision != baseline.AssetRevision {
		return out, cd.ErrConflict
	}
	out, err = s.compareConfigurationTx(ctx, tx, baseline, actual)
	if err != nil {
		return out, err
	}
	if err = automationAudit(ctx, tx, actor.User.ID, "configuration.drift.compare", "success", asset, access.CurrentMeta(ctx)); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
func (s *Store) ConfigurationDriftHistory(ctx context.Context, asset string, limit, offset int) ([]cd.Record, error) {
	if !au.Identifier(asset) || limit < 1 || limit > 200 || offset < 0 || offset > 100000 {
		return nil, cd.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT `+driftRecordColumns+` FROM configuration_drift_results WHERE asset_id=$1 ORDER BY id DESC LIMIT $2 OFFSET $3`, asset, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []cd.Record{}
	for rows.Next() {
		v, e := readDriftRecord(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) ConfigurationDrift(ctx context.Context, asset string) (cd.State, error) {
	maxAge := s.configurationDriftMaxAge
	if maxAge == 0 {
		maxAge = cd.DefaultMaxAge
	}
	out := cd.State{AssetID: asset, Status: "unknown", Reason: "baseline_missing", MaxAgeSeconds: int(maxAge.Seconds())}
	var revision int64
	var lifecycle, scope string
	if err := s.pool.QueryRow(ctx, `SELECT revision,lifecycle_status,scope FROM assets WHERE id=$1`, asset).Scan(&revision, &lifecycle, &scope); errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	} else if err != nil {
		return out, err
	}
	baseline, err := s.ConfigurationBaseline(ctx, asset)
	if errors.Is(err, ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Baseline = &baseline
	record, err := readDriftRecord(s.pool.QueryRow(ctx, `SELECT `+driftRecordColumns+` FROM configuration_drift_results WHERE asset_id=$1 AND baseline_revision=$2 ORDER BY actual_observed_at DESC,id DESC LIMIT 1`, asset, baseline.Revision))
	if errors.Is(err, ErrNotFound) {
		out.Reason = "actual_snapshot_missing"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Latest = &record
	if scope != "" || lifecycle != "observed" || revision != baseline.AssetRevision {
		out.Reason = "asset_changed_since_approval"
		return out, nil
	}
	var targetRevision int64
	var enabled bool
	var template string
	err = s.pool.QueryRow(ctx, `SELECT revision,(definition->>'enabled')::boolean,definition->>'template_id' FROM automation_targets WHERE asset_id=$1`, asset).Scan(&targetRevision, &enabled, &template)
	if errors.Is(err, pgx.ErrNoRows) {
		out.Reason = "automation_target_missing"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if !enabled || targetRevision != record.ActualTargetRevision || template != baseline.ReadTemplateID {
		out.Reason = "target_changed_or_disabled"
		return out, nil
	}
	now := time.Now()
	out.Fresh = !record.ActualObservedAt.After(now.Add(30*time.Second)) && record.ActualObservedAt.After(now.Add(-maxAge))
	if !out.Fresh {
		out.Reason = "actual_snapshot_stale"
		return out, nil
	}
	out.Status = record.Status
	out.Reason = record.ErrorCode
	if out.Status == "compliant" {
		out.Reason = "matches_approved_baseline"
	} else if out.Status == "drift" {
		out.Reason = "differs_from_approved_baseline"
	}
	return out, nil
}
func (s *Store) ConfigureConfigurationDrift(maxAge time.Duration) error {
	if maxAge < time.Minute || maxAge > 24*time.Hour {
		return cd.ErrInvalid
	}
	s.configurationDriftMaxAge = maxAge
	return nil
}
