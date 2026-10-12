package storage

import (
	"context"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	invmodel "github.com/InfraPulse2026/InfraPulse-Backend/internal/inventory"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/locations"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
)

type ResolvedInventory struct {
	invmodel.Summary
	Placement locations.Placement `json:"placement"`
}

func (s *Store) ResolvedInventory(ctx context.Context, id string) (ResolvedInventory, error) {
	var out ResolvedInventory
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	a, err := readAsset(tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets a WHERE id=$1`, id))
	if err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT b.id,b.source_instance_id,s.type,s.name,b.provenance,b.latest_fact_id,b.inventory_at,f.received_at,s.enabled,b.inventory,b.provider_state FROM asset_source_bindings b JOIN data_sources s ON s.id=b.source_instance_id JOIN asset_fact_versions f ON f.id=b.latest_fact_id WHERE b.asset_id=$1 ORDER BY b.id LIMIT 129`, id)
	if err != nil {
		return out, err
	}
	snapshots := []invmodel.Snapshot{}
	total := 0
	for rows.Next() {
		var v invmodel.Snapshot
		var raw []byte
		if err = rows.Scan(&v.BindingID, &v.SourceID, &v.SourceType, &v.SourceName, &v.Provenance, &v.FactID, &v.ObservedAt, &v.ReceivedAt, &v.SourceEnabled, &raw, &v.ProviderState); err != nil {
			rows.Close()
			return out, err
		}
		total += len(raw) + len(v.ProviderState)
		if len(snapshots) >= 128 || total > 8<<20 {
			rows.Close()
			return out, assets.ErrConflict
		}
		v.Device = &pb.Device{}
		if err = protojson.Unmarshal(raw, v.Device); err != nil {
			rows.Close()
			return out, err
		}
		snapshots = append(snapshots, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	out.Summary = invmodel.Build(a, snapshots)
	out.Placement, err = placementTx(ctx, tx, id)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
