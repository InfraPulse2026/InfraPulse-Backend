package storage

import (
	"context"
	"errors"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/jackc/pgx/v5"
)

const assetColumns = `a.id,a.kind,a.canonical_name,a.vendor,a.model,a.serial,a.hardware_revision,a.software_version,a.firmware_version,a.lifecycle_status,a.resolution_status,a.scope,COALESCE(a.canonical_binding_id,''),(SELECT count(*) FROM asset_source_bindings b WHERE b.asset_id=a.id),a.created_at,a.updated_at,a.revision,a.merged_into_id`

func readAsset(row interface{ Scan(...any) error }) (assets.Asset, error) {
	var a assets.Asset
	err := row.Scan(&a.ID, &a.Kind, &a.CanonicalName, &a.Vendor, &a.Model, &a.Serial, &a.HardwareRevision, &a.SoftwareVersion, &a.FirmwareVersion, &a.LifecycleStatus, &a.ResolutionStatus, &a.Scope, &a.CanonicalBindingID, &a.SourceCount, &a.CreatedAt, &a.UpdatedAt, &a.Revision, &a.MergedIntoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func (s *Store) Assets(ctx context.Context, kind, scope string, limit, offset int) ([]assets.Asset, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+assetColumns+` FROM assets a WHERE a.lifecycle_status='observed' AND ($1='' OR kind=$1) AND ($2='all' OR ($2='real' AND scope='') OR ($2='simulated' AND scope<>'')) ORDER BY a.created_at,a.id LIMIT $3 OFFSET $4`, kind, scope, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []assets.Asset{}
	for rows.Next() {
		a, e := readAsset(rows)
		if e != nil {
			return nil, e
		}
		items = append(items, a)
	}
	return items, rows.Err()
}
func (s *Store) Asset(ctx context.Context, id string) (assets.Asset, error) {
	return readAsset(s.pool.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets a WHERE a.id=$1`, id))
}

func (s *Store) AssetBindings(ctx context.Context, id string, inventory bool, limit, offset int) ([]assets.Binding, error) {
	if _, err := s.Asset(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT b.id,b.asset_id,b.source_instance_id,s.type,s.name,b.external_type,b.external_id,b.confidence,b.provenance,b.scope,b.legacy_agent_id,b.legacy_device_id,b.first_seen,b.last_seen,b.inventory_at,COALESCE(b.latest_fact_id,0),CASE WHEN $2 THEN b.inventory ELSE NULL END,b.provider_state FROM asset_source_bindings b JOIN data_sources s ON s.id=b.source_instance_id WHERE b.asset_id=$1 ORDER BY b.id LIMIT $3 OFFSET $4`, id, inventory, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []assets.Binding{}
	for rows.Next() {
		var b assets.Binding
		err = rows.Scan(&b.ID, &b.AssetID, &b.SourceInstanceID, &b.SourceType, &b.SourceName, &b.ExternalType, &b.ExternalID, &b.Confidence, &b.Provenance, &b.Scope, &b.LegacyAgentID, &b.LegacyDeviceID, &b.FirstSeen, &b.LastSeen, &b.InventoryAt, &b.LatestFactID, &b.Inventory, &b.ProviderState)
		if err != nil {
			return nil, err
		}
		items = append(items, b)
	}
	return items, rows.Err()
}

func (s *Store) AssetFacts(ctx context.Context, id string, limit, offset int) ([]assets.FactVersion, error) {
	if _, err := s.Asset(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT f.id,f.binding_id,b.asset_id,f.observed_at,f.received_at,f.origin,f.origin_ref,f.id=b.latest_fact_id,f.inventory,f.provider_state FROM asset_fact_versions f JOIN asset_source_bindings b ON b.id=f.binding_id WHERE b.asset_id=$1 ORDER BY f.observed_at DESC,f.id DESC LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []assets.FactVersion{}
	for rows.Next() {
		var f assets.FactVersion
		if err = rows.Scan(&f.ID, &f.BindingID, &f.AssetID, &f.ObservedAt, &f.ReceivedAt, &f.Origin, &f.OriginRef, &f.Current, &f.Inventory, &f.ProviderState); err != nil {
			return nil, err
		}
		items = append(items, f)
	}
	return items, rows.Err()
}

func (s *Store) AssetIdentities(ctx context.Context, id string, limit, offset int) ([]assets.Identity, error) {
	if _, err := s.Asset(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT i.id,i.binding_id,i.fact_id,i.kind,i.value,i.normalized_value,i.confidence,i.subject_id,i.usable,i.fact_id=b.latest_fact_id,i.evidence,f.observed_at FROM asset_identity_evidence i JOIN asset_source_bindings b ON b.id=i.binding_id JOIN asset_fact_versions f ON f.id=i.fact_id WHERE b.asset_id=$1 ORDER BY i.id DESC LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []assets.Identity{}
	for rows.Next() {
		var i assets.Identity
		if err = rows.Scan(&i.ID, &i.BindingID, &i.FactID, &i.Kind, &i.Value, &i.NormalizedValue, &i.Confidence, &i.SubjectID, &i.Usable, &i.Current, &i.Evidence, &i.ObservedAt); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	return items, rows.Err()
}
