package storage

import (
	"context"
	"encoding/json"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/jackc/pgx/v5"
)

// ConfigureAssetResolution is startup configuration; existing manual mappings
// and all simulated namespaces remain outside automatic reassignment.
func (s *Store) ConfigureAssetResolution(enabled bool, maxAge time.Duration) {
	s.autoAssetResolution = enabled
	s.assetResolutionAge = maxAge
}
func markAssetProjection(ctx context.Context, tx pgx.Tx, agent, message, status string) error {
	_, err := tx.Exec(ctx, `INSERT INTO asset_projection_marks(agent_id,message_id,status) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, agent, message, status)
	return err
}
func (s *Store) autoFactFresh(f assetBindingFact) bool {
	age := s.assetResolutionAge
	if age <= 0 {
		age = 24 * time.Hour
	}
	quality := f.Device.GetObservation().GetTime().GetClockQuality()
	for _, i := range assets.Identities(f.Device) {
		if i.SubjectID != "" || !i.Usable || i.Confidence != "high" || i.Kind != "serial" && i.Kind != "hardware_uuid" || len(i.Evidence) == 0 {
			continue
		}
		var e pb.IdentityEvidence
		if protojson.Unmarshal(i.Evidence, &e) != nil {
			return false
		}
		if at := e.Time; at != nil && (at.ObservedAt == nil || at.ObservedAt.AsTime().Before(time.Now().Add(-age)) || at.ObservedAt.AsTime().After(time.Now().Add(30*time.Second)) || at.ClockQuality == "degraded" || at.ClockQuality == "unsynced") {
			return false
		}
	}
	return !f.ObservedAt.Before(time.Now().Add(-age)) && !f.ObservedAt.After(time.Now().Add(30*time.Second)) && quality != "degraded" && quality != "unsynced"
}
func resolutionEvent(ctx context.Context, tx pgx.Tx, f assetBindingFact, source, target, code string, targetFacts []int64) error {
	if targetFacts == nil {
		targetFacts = []int64{}
	}
	raw, _ := json.Marshal(targetFacts)
	observed := []time.Time{}
	if len(targetFacts) > 0 {
		rows, e := tx.Query(ctx, `SELECT inventory_at FROM asset_source_bindings WHERE latest_fact_id=ANY($1::bigint[]) ORDER BY id`, targetFacts)
		if e != nil {
			return e
		}
		for rows.Next() {
			var at time.Time
			if e = rows.Scan(&at); e != nil {
				rows.Close()
				return e
			}
			observed = append(observed, at)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
	}
	observedJSON, _ := json.Marshal(observed)
	var targetID any
	if target != "" {
		targetID = target
	}
	_, err := tx.Exec(ctx, `INSERT INTO asset_resolution_events(binding_id,source_asset_id,target_asset_id,fact_id,target_fact_ids,code,source_observed_at,target_observed_ats) SELECT $1,$2,$3,$4,$5,$6,$7,$8 WHERE NOT EXISTS(SELECT 1 FROM asset_resolution_events WHERE id=(SELECT max(id) FROM asset_resolution_events WHERE binding_id=$1) AND fact_id=$4 AND code=$6 AND target_asset_id IS NOT DISTINCT FROM $3 AND target_fact_ids=$5::jsonb)`, f.ID, source, targetID, f.FactID, raw, code, f.ObservedAt, observedJSON)
	return err
}
func (s *Store) resolveSourceObject(ctx context.Context, tx pgx.Tx, sourceID, externalType, externalID, scope string) error {
	if !s.autoAssetResolution || scope != "" {
		return nil
	}
	var assetID, confidence string
	if err := tx.QueryRow(ctx, `SELECT asset_id,confidence FROM asset_source_bindings WHERE source_instance_id=$1 AND external_type=$2 AND external_id=$3 AND scope=$4`, sourceID, externalType, externalID, scope).Scan(&assetID, &confidence); err != nil {
		return err
	}
	if confidence != "source_owned" {
		return nil
	}
	a, err := readAsset(tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets a WHERE id=$1`, assetID))
	if err != nil {
		return err
	}
	if a.SourceCount != 1 || a.Kind == "unknown" || a.ResolutionStatus == "conflict" || a.LifecycleStatus != "observed" {
		return nil
	}
	var pinned bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM asset_decisions WHERE target_id=$1 OR source_id=$1) OR EXISTS(SELECT 1 FROM asset_placements WHERE asset_id=$1 AND mode<>'unplaced')`, assetID).Scan(&pinned); err != nil {
		return err
	}
	if pinned {
		return nil
	}
	incoming, err := assetBindingFacts(ctx, tx, assetID)
	if err != nil {
		return err
	}
	if len(incoming) != 1 {
		return nil
	}
	f := incoming[0]
	if !s.autoFactFresh(f) {
		return nil
	}
	keysUUID, keysSerial := f.Signature.StrongUUIDs, f.Signature.StrongSerials
	if len(keysUUID) == 0 && len(keysSerial) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT a.id FROM assets a JOIN asset_source_bindings b ON b.asset_id=a.id JOIN asset_identity_evidence i ON i.binding_id=b.id AND i.fact_id=b.latest_fact_id WHERE a.id<>$1 AND a.scope='' AND a.kind=$2 AND a.lifecycle_status='observed' AND i.usable AND i.subject_id='' AND i.confidence='high' AND ((i.kind='hardware_uuid' AND i.normalized_value=ANY($3::text[])) OR (i.kind='serial' AND i.normalized_value=ANY($4::text[]))) ORDER BY a.id LIMIT 3`, assetID, a.Kind, keysUUID, keysSerial)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > 1 {
		return resolutionEvent(ctx, tx, f, assetID, "", "ambiguous", nil)
	}
	targetID := ids[0]
	target, err := assetBindingFacts(ctx, tx, targetID)
	if err != nil {
		return err
	}
	proof := []int64{}
	for _, other := range target {
		proof = append(proof, other.FactID)
	}
	var separated bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM asset_separations WHERE asset_a=LEAST($1,$2) AND asset_b=GREATEST($1,$2))`, assetID, targetID).Scan(&separated); err != nil {
		return err
	}
	if separated {
		return resolutionEvent(ctx, tx, f, assetID, targetID, "separated", proof)
	}
	if len(target) >= 128 || bindingConflicts(append(append([]assetBindingFact{}, target...), f)) {
		return resolutionEvent(ctx, tx, f, assetID, targetID, "conflict", proof)
	}
	qualified := false
	for _, other := range target {
		if other.SourceID == sourceID {
			return resolutionEvent(ctx, tx, f, assetID, targetID, "insufficient_evidence", proof)
		}
		if !s.autoFactFresh(other) {
			return resolutionEvent(ctx, tx, f, assetID, targetID, "stale", proof)
		}
		qualified = qualified || assets.AutoCompatible(f.Signature, other.Signature)
	}
	if !qualified {
		return resolutionEvent(ctx, tx, f, assetID, targetID, "insufficient_evidence", proof)
	}
	if _, err = tx.Exec(ctx, `UPDATE assets SET canonical_binding_id=NULL WHERE id=$1`, assetID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE asset_source_bindings SET asset_id=$2,confidence='automatic' WHERE id=$1`, f.ID, targetID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE assets SET lifecycle_status='merged',merged_into_id=$2,revision=revision+1,updated_at=now() WHERE id=$1`, assetID, targetID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE assets SET revision=revision+1,updated_at=now() WHERE id=$1`, targetID); err != nil {
		return err
	}
	if err = refreshAssetResolution(ctx, tx, targetID); err != nil {
		return err
	}
	return resolutionEvent(ctx, tx, f, assetID, targetID, "matched", proof)
}

func (s *Store) AssetResolutionEvents(ctx context.Context, id string, limit, offset int) ([]assets.ResolutionEvent, error) {
	if _, err := s.Asset(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,binding_id,source_asset_id,target_asset_id,fact_id,target_fact_ids,code,created_at,source_observed_at,target_observed_ats FROM asset_resolution_events WHERE source_asset_id=$1 OR target_asset_id=$1 ORDER BY id DESC LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []assets.ResolutionEvent{}
	for rows.Next() {
		var e assets.ResolutionEvent
		var proof, observed []byte
		if err = rows.Scan(&e.ID, &e.BindingID, &e.SourceAssetID, &e.TargetAssetID, &e.FactID, &proof, &e.Code, &e.CreatedAt, &e.SourceObservedAt, &observed); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(proof, &e.TargetFactIDs); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(observed, &e.TargetObservedAts); err != nil {
			return nil, err
		}
		items = append(items, e)
	}
	return items, rows.Err()
}
