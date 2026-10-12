package storage

import (
	"context"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/jackc/pgx/v5"
)

// Candidates are explanations for review, never automatic merge instructions.
func (s *Store) AssetCandidates(ctx context.Context, id string, limit int) ([]assets.Candidate, error) {
	if limit < 1 || limit > 50 {
		return nil, assets.ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	a, err := readAsset(tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets a WHERE id=$1`, id))
	if err != nil {
		return nil, err
	}
	items := []assets.Candidate{}
	if a.LifecycleStatus != "observed" || a.Kind == "unknown" {
		return items, tx.Commit(ctx)
	}
	root, err := assetBindingFacts(ctx, tx, a.ID)
	if err != nil {
		return nil, err
	}
	uuids, serials := []string{}, []string{}
	for _, f := range root {
		uuids = append(uuids, f.Signature.StrongUUIDs...)
		serials = append(serials, f.Signature.StrongSerials...)
	}
	if len(uuids) == 0 && len(serials) == 0 {
		return items, tx.Commit(ctx)
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT a.id FROM assets a JOIN asset_source_bindings b ON b.asset_id=a.id JOIN asset_identity_evidence i ON i.binding_id=b.id AND i.fact_id=b.latest_fact_id WHERE a.id<>$1 AND a.scope=$2 AND a.kind=$3 AND a.lifecycle_status='observed' AND i.usable AND i.subject_id='' AND i.confidence='high' AND ((i.kind='hardware_uuid' AND i.normalized_value=ANY($4::text[])) OR (i.kind='serial' AND i.normalized_value=ANY($5::text[]))) ORDER BY a.id LIMIT $6`, a.ID, a.Scope, a.Kind, uuids, serials, limit)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, candidateID := range ids {
		b, e := readAsset(tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets a WHERE id=$1`, candidateID))
		if e != nil {
			return nil, e
		}
		other, e := assetBindingFacts(ctx, tx, b.ID)
		if e != nil {
			return nil, e
		}
		comparison := assets.Comparison{Matches: []string{}, Conflicts: []string{}}
		matchSet, conflictSet := map[string]bool{}, map[string]bool{}
		for _, left := range root {
			for _, right := range other {
				c := assets.Compare(left.Signature, right.Signature)
				comparison.StrongMatch = comparison.StrongMatch || c.StrongMatch
				for _, m := range c.Matches {
					if !matchSet[m] {
						matchSet[m] = true
						comparison.Matches = append(comparison.Matches, m)
					}
				}
				for _, v := range c.Conflicts {
					if !conflictSet[v] {
						conflictSet[v] = true
						comparison.Conflicts = append(comparison.Conflicts, v)
					}
				}
			}
		}
		if len(comparison.Conflicts) > 0 || bindingConflicts(root) || bindingConflicts(other) {
			comparison.StrongMatch = false
			if len(comparison.Conflicts) == 0 {
				comparison.Conflicts = append(comparison.Conflicts, "inconsistent_source_group")
			}
		}
		var blocked bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM asset_separations WHERE asset_a=LEAST($1,$2) AND asset_b=GREATEST($1,$2))`, a.ID, b.ID).Scan(&blocked); e != nil {
			return nil, e
		}
		items = append(items, assets.Candidate{Asset: b, Comparison: comparison, BlockedByDecision: blocked})
	}
	return items, tx.Commit(ctx)
}
