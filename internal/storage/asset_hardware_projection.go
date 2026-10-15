package storage

import (
	"context"
	"encoding/json"
	"errors"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assethardware"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
)

const assetHardwareUpsert = `WITH added AS(INSERT INTO asset_hardware_samples(binding_id,source_instance_id,observation_key,agent_id,message_id,sample_index,kind,observed_at,payload,received_at) SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,received_at FROM agent_messages WHERE agent_id=$4 AND message_id=$5 ON CONFLICT(agent_id,message_id,sample_index) DO NOTHING RETURNING id) INSERT INTO asset_hardware_latest(observation_key,sample_id,binding_id,source_instance_id,kind,observed_at) SELECT $3,id,$1,$2,$7,$8 FROM added WHERE $8<=now()+interval '30 seconds' ON CONFLICT(observation_key) DO UPDATE SET sample_id=EXCLUDED.sample_id,observed_at=EXCLUDED.observed_at WHERE EXCLUDED.observed_at>=asset_hardware_latest.observed_at`

func projectCollectorHardware(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope) error {
	observations, err := assethardware.Extract(e)
	if err != nil || len(observations) == 0 {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194030)`); err != nil {
		return err
	}
	source, err := collectorAssetSource(ctx, tx, e.AgentId)
	if err != nil {
		return err
	}
	type ownerBinding struct {
		id     string
		device *pb.Device
	}
	owners := map[string]ownerBinding{}
	batch := &pgx.Batch{}
	flush := func() error {
		if batch.Len() == 0 {
			return nil
		}
		err := tx.SendBatch(ctx, batch).Close()
		batch = &pgx.Batch{}
		return err
	}
	for i, o := range observations {
		scope := ""
		if o.Provenance == "simulated" {
			scope = e.SessionId
		}
		ownerKey := o.DeviceID + "\x00" + scope
		owner, known := owners[ownerKey]
		if !known {
			var inventoryRaw []byte
			err = tx.QueryRow(ctx, `SELECT id,inventory FROM asset_source_bindings WHERE source_instance_id=$1 AND external_type='device' AND external_id=$2 AND scope=$3 AND legacy_agent_id=$4`, source, o.DeviceID, scope, e.AgentId).Scan(&owner.id, &inventoryRaw)
			// Missing inventory and a different target are kept in the immutable archive;
			// neither may attach to a similarly named foreign asset.
			if errors.Is(err, pgx.ErrNoRows) {
				owners[ownerKey] = ownerBinding{}
				continue
			}
			if err != nil {
				return err
			}
			owner.device = &pb.Device{}
			if err = protojson.Unmarshal(inventoryRaw, owner.device); err != nil {
				return err
			}
			owners[ownerKey] = owner
		}
		if owner.device == nil || !assethardware.CompatibleSource(o, owner.device) {
			continue
		}
		raw, err := json.Marshal(o)
		if err != nil {
			return err
		}
		batch.Queue(assetHardwareUpsert, owner.id, source, o.Key(owner.id), e.AgentId, e.MessageId, i, o.Kind, o.ObservedAt, raw)
		if batch.Len() >= 256 {
			if err = flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// Seed accepted current inventory and one latest hardware telemetry envelope per
// current source binding. The complete historical inbox is not copied at startup.
func seedAssetHardware(ctx context.Context, tx pgx.Tx) error {
	lastBinding := ""
	for {
		rows, err := tx.Query(ctx, `SELECT b.id,m.payload,t.payload FROM asset_source_bindings b
LEFT JOIN asset_fact_versions f ON f.id=b.latest_fact_id
LEFT JOIN agent_messages m ON m.agent_id=b.legacy_agent_id AND m.message_id=f.origin_ref AND m.kind IN ('hello','inventory')
LEFT JOIN LATERAL (SELECT am.payload FROM telemetry sample JOIN agent_messages am ON am.agent_id=sample.agent_id AND am.message_id=sample.message_id WHERE sample.agent_id=b.legacy_agent_id AND sample.device_id=b.legacy_device_id
AND sample.observed_at<=now()+interval '30 seconds'
AND (b.scope='' AND COALESCE(sample.payload->>'source','')<>'simulated' AND COALESCE(sample.payload->'observation'->'source'->>'provenance','')<>'simulated' OR b.scope<>'' AND am.session_id=b.scope)
AND (COALESCE(jsonb_array_length(sample.payload->'hardware_health'),0)>0 OR EXISTS(SELECT 1 FROM jsonb_array_elements(COALESCE(sample.payload->'metrics','[]')) metric WHERE metric->>'name' LIKE 'hardware.%' OR metric->>'resource_id'<>'' OR metric->>'component_id'<>''))
AND (b.inventory->'observation'->'source' IS NULL OR (COALESCE(sample.payload->'observation'->'source'->>'type','')=COALESCE(b.inventory->'observation'->'source'->>'type','') AND COALESCE(sample.payload->'observation'->'source'->>'target_id','')=COALESCE(b.inventory->'observation'->'source'->>'target_id','') AND COALESCE(sample.payload->'observation'->'source'->>'protocol','')=COALESCE(b.inventory->'observation'->'source'->>'protocol','') AND COALESCE(sample.payload->'observation'->'source'->>'instance_id','')=COALESCE(b.inventory->'observation'->'source'->>'instance_id','')))
ORDER BY sample.observed_at DESC,sample.id DESC LIMIT 1) t ON true
WHERE b.id>$1 AND b.legacy_agent_id IS NOT NULL ORDER BY b.id LIMIT 100`, lastBinding)
		if err != nil {
			return err
		}
		type archived struct {
			binding              string
			inventory, telemetry []byte
		}
		page := []archived{}
		for rows.Next() {
			var r archived
			if err = rows.Scan(&r.binding, &r.inventory, &r.telemetry); err != nil {
				rows.Close()
				return err
			}
			page = append(page, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, r := range page {
			for _, raw := range [][]byte{r.inventory, r.telemetry} {
				if len(raw) == 0 {
					continue
				}
				var e pb.AgentEnvelope
				if err = protojson.Unmarshal(raw, &e); err != nil {
					return err
				}
				if err = projectCollectorHardware(ctx, tx, &e); err != nil {
					return err
				}
			}
			lastBinding = r.binding
		}
	}
}
