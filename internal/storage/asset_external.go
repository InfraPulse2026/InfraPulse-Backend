package storage

import (
	"context"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/jackc/pgx/v5"
)

func validateAssetObservations(observations []assets.Observation) bool {
	if len(observations) > 200 {
		return false
	}
	total := 0
	seen := map[string]bool{}
	for _, o := range observations {
		if !assets.ValidObservation(o) || o.ObservedAt.After(time.Now().Add(30*time.Second)) {
			return false
		}
		key := o.ExternalType + "\x00" + o.ExternalID + "\x00" + o.Scope
		if seen[key] {
			return false
		}
		seen[key] = true
		raw, err := wireJSON(o.Device)
		if err != nil || len(raw) > 1<<20 {
			return false
		}
		total += len(raw) + len(o.ProviderState)
		if total > 4<<20 {
			return false
		}
	}
	return true
}
func (s *Store) projectExternalAssets(ctx context.Context, tx pgx.Tx, sourceID, jobID string, observations []assets.Observation) error {
	for _, o := range observations {
		if err := projectAssetObservation(ctx, tx, sourceID, "", o.Scope, jobID, "external_sync", o.ExternalType, o.Provenance, o.Device, o.ObservedAt, o.ProviderState); err != nil {
			return err
		}
		if err := s.resolveSourceObject(ctx, tx, sourceID, o.ExternalType, o.ExternalID, o.Scope); err != nil {
			return err
		}
	}
	return nil
}
