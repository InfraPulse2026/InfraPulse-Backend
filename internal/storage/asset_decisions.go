package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
)

type assetBindingFact struct {
	ID         string
	SourceID   string
	FactID     int64
	ObservedAt time.Time
	Device     *pb.Device
	Signature  assets.Signature
}

func assetBindingFacts(ctx context.Context, tx pgx.Tx, id string) ([]assetBindingFact, error) {
	rows, err := tx.Query(ctx, `SELECT id,inventory,source_instance_id,COALESCE(latest_fact_id,0),inventory_at FROM asset_source_bindings WHERE asset_id=$1 ORDER BY id LIMIT 129`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []assetBindingFact{}
	for rows.Next() {
		var f assetBindingFact
		var raw []byte
		if err = rows.Scan(&f.ID, &raw, &f.SourceID, &f.FactID, &f.ObservedAt); err != nil {
			return nil, err
		}
		f.Device = &pb.Device{}
		if err = protojson.Unmarshal(raw, f.Device); err != nil {
			return nil, assets.ErrConflict
		}
		f.Signature = assets.RootSignature(f.Device)
		items = append(items, f)
	}
	if len(items) > 128 {
		return nil, assets.ErrConflict
	}
	return items, rows.Err()
}
func bindingConflicts(items []assetBindingFact) bool {
	for i, a := range items {
		if len(assets.Compare(a.Signature, a.Signature).Conflicts) > 0 {
			return true
		}
		for _, b := range items[i+1:] {
			if len(assets.Compare(a.Signature, b.Signature).Conflicts) > 0 {
				return true
			}
		}
	}
	return false
}
func refreshAssetResolution(ctx context.Context, tx pgx.Tx, id string) error {
	items, err := assetBindingFacts(ctx, tx, id)
	if err != nil {
		return err
	}
	status := "unresolved"
	if len(items) > 1 {
		status = "resolved"
	}
	if bindingConflicts(items) {
		status = "conflict"
	}
	_, err = tx.Exec(ctx, `UPDATE assets SET resolution_status=$2 WHERE id=$1`, id, status)
	return err
}
func setAssetCanonical(ctx context.Context, tx pgx.Tx, id, bindingID string) error {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT inventory FROM asset_source_bindings WHERE id=$1 AND asset_id=$2`, bindingID, id).Scan(&raw); err != nil {
		return assets.ErrConflict
	}
	d := &pb.Device{}
	if protojson.Unmarshal(raw, d) != nil {
		return assets.ErrConflict
	}
	name := d.Name
	if name == "" {
		name = d.Id
	}
	_, err := tx.Exec(ctx, `UPDATE assets SET kind=$2,canonical_name=$3,vendor=$4,model=$5,serial=$6,hardware_revision=$7,software_version=$8,firmware_version=$9,canonical_binding_id=$10,revision=revision+1,updated_at=now() WHERE id=$1`, id, assets.Kind(d), name, d.Vendor, d.Model, d.SerialNumber, d.HardwareRevision, d.SoftwareVersion, d.FirmwareVersion, bindingID)
	return err
}

const decisionColumns = `id,operation,actor_id,target_id,source_id,binding_ids,canonical_binding_id,reason,created_at`

func readAssetDecision(row interface{ Scan(...any) error }) (assets.Decision, error) {
	var d assets.Decision
	var ids []byte
	err := row.Scan(&d.ID, &d.Operation, &d.ActorID, &d.TargetID, &d.SourceID, &ids, &d.CanonicalBindingID, &d.Reason, &d.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	err = json.Unmarshal(ids, &d.BindingIDs)
	return d, err
}
func assetDecisionLock(ctx context.Context, tx pgx.Tx) (access.Principal, error) {
	actor, ok := access.Current(ctx)
	if !ok {
		return actor, access.ErrForbidden
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194002)`); err != nil {
		return actor, err
	}
	if err := accessAdminTx(ctx, tx, actor); err != nil {
		return actor, err
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194030)`)
	return actor, err
}
func decisionRequestHash(operation string, input any) string {
	raw, _ := json.Marshal(input)
	sum := sha256.Sum256(append([]byte(operation+":"), raw...))
	return hex.EncodeToString(sum[:])
}
func priorAssetDecision(ctx context.Context, tx pgx.Tx, actor, key, hash string) (assets.Decision, bool, error) {
	var oldHash string
	err := tx.QueryRow(ctx, `SELECT request_hash FROM asset_decisions WHERE actor_id=$1 AND idempotency_key=$2`, actor, key).Scan(&oldHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return assets.Decision{}, false, nil
	}
	if err != nil {
		return assets.Decision{}, false, err
	}
	if oldHash != hash {
		return assets.Decision{}, false, assets.ErrConflict
	}
	d, err := readAssetDecision(tx.QueryRow(ctx, `SELECT `+decisionColumns+` FROM asset_decisions WHERE actor_id=$1 AND idempotency_key=$2`, actor, key))
	return d, true, err
}
func saveAssetDecision(ctx context.Context, tx pgx.Tx, actor access.Principal, operation, key, hash, target, source, canonical, reason string, ids []string) (assets.Decision, error) {
	id, err := assetRegistryID("decision_")
	if err != nil {
		return assets.Decision{}, err
	}
	raw, _ := json.Marshal(ids)
	_, err = tx.Exec(ctx, `INSERT INTO asset_decisions(id,operation,actor_id,idempotency_key,request_hash,target_id,source_id,binding_ids,canonical_binding_id,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, operation, actor.User.ID, key, hash, target, source, raw, canonical, logbook.SanitizeText(reason))
	if err != nil {
		return assets.Decision{}, err
	}
	meta := access.CurrentMeta(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,$2,'success',$3,$4,$5)`, actor.User.ID, "asset_"+operation, id, meta.RequestID, meta.Peer); err != nil {
		return assets.Decision{}, err
	}
	return readAssetDecision(tx.QueryRow(ctx, `SELECT `+decisionColumns+` FROM asset_decisions WHERE id=$1`, id))
}

func (s *Store) MergeAssets(ctx context.Context, in assets.MergeInput, key string) (assets.Decision, bool, error) {
	var empty assets.Decision
	if !assets.ValidMerge(in) || commands.ValidateKey(key) != nil {
		return empty, false, assets.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, false, err
	}
	defer tx.Rollback(context.Background())
	actor, err := assetDecisionLock(ctx, tx)
	if err != nil {
		return empty, false, err
	}
	hash := decisionRequestHash("merge", in)
	if d, exists, e := priorAssetDecision(ctx, tx, actor.User.ID, key, hash); e != nil || exists {
		return d, exists, e
	}
	target, err := readAsset(tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets a WHERE id=$1 FOR UPDATE`, in.TargetID))
	if err != nil {
		return empty, false, err
	}
	source, err := readAsset(tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets a WHERE id=$1 FOR UPDATE`, in.SourceID))
	if err != nil {
		return empty, false, err
	}
	if target.Revision != in.TargetRevision || source.Revision != in.SourceRevision || target.LifecycleStatus != "observed" || source.LifecycleStatus != "observed" || target.Scope != source.Scope || target.Kind != source.Kind || target.Kind == "unknown" {
		return empty, false, assets.ErrConflict
	}
	// Placement belongs to the physical Asset, not to a source binding. A merge
	// must not silently abandon an occupied rack slot on the merged alias.
	var placed bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM asset_placements WHERE asset_id=$1 AND mode<>'unplaced')`, source.ID).Scan(&placed); err != nil {
		return empty, false, err
	}
	if placed {
		return empty, false, assets.ErrConflict
	}
	a, err := assetBindingFacts(ctx, tx, target.ID)
	if err != nil {
		return empty, false, err
	}
	b, err := assetBindingFacts(ctx, tx, source.ID)
	if err != nil {
		return empty, false, err
	}
	all := append(append([]assetBindingFact{}, a...), b...)
	if len(a) == 0 || len(b) == 0 || len(all) > 128 || bindingConflicts(all) {
		return empty, false, assets.ErrConflict
	}
	canonicalFound := false
	ids := []string{}
	for _, f := range all {
		canonicalFound = canonicalFound || f.ID == in.CanonicalBindingID
	}
	if !canonicalFound {
		return empty, false, assets.ErrConflict
	}
	for _, f := range b {
		ids = append(ids, f.ID)
	}
	if _, err = tx.Exec(ctx, `UPDATE assets SET canonical_binding_id=NULL WHERE id=$1`, source.ID); err != nil {
		return empty, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE asset_source_bindings SET asset_id=$2,confidence='manual' WHERE asset_id=$1`, source.ID, target.ID); err != nil {
		return empty, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE assets SET lifecycle_status='merged',merged_into_id=$2,revision=revision+1,updated_at=now() WHERE id=$1`, source.ID, target.ID); err != nil {
		return empty, false, err
	}
	if err = setAssetCanonical(ctx, tx, target.ID, in.CanonicalBindingID); err != nil {
		return empty, false, err
	}
	if err = refreshAssetResolution(ctx, tx, target.ID); err != nil {
		return empty, false, err
	}
	// Preserve separation instructions through aliases. The reviewed pair itself
	// is cleared; unrelated do-not-merge constraints are inherited by the target.
	_, err = tx.Exec(ctx, `INSERT INTO asset_separations(asset_a,asset_b,decision_id) SELECT LEAST($2,CASE WHEN asset_a=$1 THEN asset_b ELSE asset_a END),GREATEST($2,CASE WHEN asset_a=$1 THEN asset_b ELSE asset_a END),decision_id FROM asset_separations WHERE (asset_a=$1 OR asset_b=$1) AND asset_a<>$2 AND asset_b<>$2 ON CONFLICT DO NOTHING`, source.ID, target.ID)
	if err != nil {
		return empty, false, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM asset_separations WHERE asset_a=$1 OR asset_b=$1`, source.ID); err != nil {
		return empty, false, err
	}
	d, err := saveAssetDecision(ctx, tx, actor, "merge", key, hash, target.ID, source.ID, in.CanonicalBindingID, in.Reason, ids)
	if err != nil {
		return empty, false, err
	}
	return d, false, tx.Commit(ctx)
}

func (s *Store) SplitAsset(ctx context.Context, in assets.SplitInput, key string) (assets.Decision, bool, error) {
	var empty assets.Decision
	if !assets.ValidSplit(in) || commands.ValidateKey(key) != nil {
		return empty, false, assets.ErrInvalid
	}
	in.BindingIDs = append([]string{}, in.BindingIDs...)
	sort.Strings(in.BindingIDs)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, false, err
	}
	defer tx.Rollback(context.Background())
	actor, err := assetDecisionLock(ctx, tx)
	if err != nil {
		return empty, false, err
	}
	hash := decisionRequestHash("split", in)
	if d, exists, e := priorAssetDecision(ctx, tx, actor.User.ID, key, hash); e != nil || exists {
		return d, exists, e
	}
	old, err := readAsset(tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets a WHERE id=$1 FOR UPDATE`, in.AssetID))
	if err != nil {
		return empty, false, err
	}
	if old.Revision != in.Revision || old.LifecycleStatus != "observed" {
		return empty, false, assets.ErrConflict
	}
	all, err := assetBindingFacts(ctx, tx, old.ID)
	if err != nil {
		return empty, false, err
	}
	selected := map[string]bool{}
	for _, id := range in.BindingIDs {
		selected[id] = true
	}
	moving, remaining := []assetBindingFact{}, []assetBindingFact{}
	for _, f := range all {
		if selected[f.ID] {
			moving = append(moving, f)
		} else {
			remaining = append(remaining, f)
		}
	}
	if len(moving) != len(selected) || len(remaining) == 0 || bindingConflicts(moving) {
		return empty, false, assets.ErrConflict
	}
	newID, err := assetRegistryID("asset_")
	if err != nil {
		return empty, false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO assets(id,kind,canonical_name,scope) VALUES($1,'unknown','',$2)`, newID, old.Scope); err != nil {
		return empty, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE assets SET canonical_binding_id=NULL WHERE id=$1`, old.ID); err != nil {
		return empty, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE asset_source_bindings SET asset_id=$2,confidence='manual' WHERE asset_id=$1 AND id=ANY($3::text[])`, old.ID, newID, in.BindingIDs); err != nil {
		return empty, false, err
	}
	oldCanonical := old.CanonicalBindingID
	if selected[oldCanonical] || oldCanonical == "" {
		oldCanonical = remaining[0].ID
	}
	if err = setAssetCanonical(ctx, tx, old.ID, oldCanonical); err != nil {
		return empty, false, err
	}
	if err = setAssetCanonical(ctx, tx, newID, in.CanonicalBindingID); err != nil {
		return empty, false, err
	}
	if err = refreshAssetResolution(ctx, tx, old.ID); err != nil {
		return empty, false, err
	}
	if err = refreshAssetResolution(ctx, tx, newID); err != nil {
		return empty, false, err
	}
	d, err := saveAssetDecision(ctx, tx, actor, "split", key, hash, newID, old.ID, in.CanonicalBindingID, in.Reason, in.BindingIDs)
	if err != nil {
		return empty, false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO asset_separations(asset_a,asset_b,decision_id) SELECT LEAST($2,CASE WHEN asset_a=$1 THEN asset_b ELSE asset_a END),GREATEST($2,CASE WHEN asset_a=$1 THEN asset_b ELSE asset_a END),decision_id FROM asset_separations WHERE asset_a=$1 OR asset_b=$1 ON CONFLICT DO NOTHING`, old.ID, newID); err != nil {
		return empty, false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO asset_separations(asset_a,asset_b,decision_id) VALUES(LEAST($1,$2),GREATEST($1,$2),$3) ON CONFLICT DO NOTHING`, old.ID, newID, d.ID); err != nil {
		return empty, false, err
	}
	return d, false, tx.Commit(ctx)
}

func (s *Store) AssetDecisions(ctx context.Context, id string, limit, offset int) ([]assets.Decision, error) {
	if _, err := s.Asset(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+decisionColumns+` FROM asset_decisions WHERE target_id=$1 OR source_id=$1 ORDER BY created_at DESC,id LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []assets.Decision{}
	for rows.Next() {
		d, e := readAssetDecision(rows)
		if e != nil {
			return nil, e
		}
		items = append(items, d)
	}
	return items, rows.Err()
}
