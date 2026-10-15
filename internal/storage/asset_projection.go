package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/jackc/pgx/v5"
)

func assetRegistryID(prefix string) (string, error) {
	id, err := access.NewID()
	return prefix + strings.TrimPrefix(id, "usr_"), err
}

func collectorAssetSource(ctx context.Context, tx pgx.Tx, agentID string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM data_sources WHERE type='COLLECTOR' AND connection_ref=$1`, agentID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	sum := sha256.Sum256([]byte(agentID))
	id = "src_collector_" + hex.EncodeToString(sum[:16])
	var agentName string
	if err = tx.QueryRow(ctx, `SELECT name FROM agents WHERE agent_id=$1`, agentID).Scan(&agentName); err != nil {
		return "", err
	}
	var label strings.Builder
	for _, r := range strings.TrimSpace(agentName) {
		if unicode.IsControl(r) {
			continue
		}
		if label.Len()+utf8.RuneLen(r) > 100 {
			break
		}
		label.WriteRune(r)
	}
	if label.Len() == 0 {
		label.WriteString(hex.EncodeToString(sum[:6]))
	}
	result, err := tx.Exec(ctx, `INSERT INTO data_sources(id,type,name,connection_ref,enabled,sync_interval_seconds,configuration,connection_status) VALUES($1,'COLLECTOR',$2,$3,true,3,'{"timeout_seconds":15}','unverified') ON CONFLICT(connection_ref) WHERE type='COLLECTOR' DO NOTHING`, id, "Collector "+label.String(), agentID)
	if err != nil {
		return "", err
	}
	if result.RowsAffected() == 1 {
		if _, err = tx.Exec(ctx, `INSERT INTO data_source_audit(source_id,action,revision) VALUES($1,'created',1)`, id); err != nil {
			return "", err
		}
	}
	err = tx.QueryRow(ctx, `SELECT id FROM data_sources WHERE type='COLLECTOR' AND connection_ref=$1`, agentID).Scan(&id)
	return id, err
}

func (s *Store) projectCollectorAssets(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope) error {
	var devices []*pb.Device
	var observed time.Time
	var origin string
	switch p := e.Payload.(type) {
	case *pb.AgentEnvelope_Hello:
		devices = []*pb.Device{p.Hello.Device}
		observed = at(p.Hello.Device.LastSeen, e.SentAt.AsTime())
		origin = "collector_hello"
	case *pb.AgentEnvelope_Inventory:
		devices = p.Inventory.Devices
		observed = p.Inventory.ObservedAt.AsTime()
		origin = "collector_inventory"
	default:
		return nil
	}
	if len(devices) == 0 || observed.After(time.Now().Add(30*time.Second)) {
		return markAssetProjection(ctx, tx, e.AgentId, e.MessageId, "skipped")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73194030)`); err != nil {
		return err
	}
	sourceID, err := collectorAssetSource(ctx, tx, e.AgentId)
	if err != nil {
		return err
	}
	for _, d := range devices {
		scope := ""
		if assets.Provenance(d) == "simulated" {
			scope = e.SessionId
		}
		if err = projectAssetObservation(ctx, tx, sourceID, e.AgentId, scope, e.MessageId, origin, "device", assets.Provenance(d), d, observed, nil); err != nil {
			return err
		}
		if err = s.resolveSourceObject(ctx, tx, sourceID, "device", d.Id, scope); err != nil {
			return err
		}
	}
	return markAssetProjection(ctx, tx, e.AgentId, e.MessageId, "projected")
}

func projectAssetObservation(ctx context.Context, tx pgx.Tx, sourceID, agentID, scope, originRef, origin, externalType, provenance string, d *pb.Device, observed time.Time, provider json.RawMessage) error {
	// Serialize creation/update of this source-owned object. Global resolution
	// is a later step; no cross-source name/IP/serial guessing takes place here.
	lockKey := sha256.Sum256([]byte(sourceID + "\x00" + externalType + "\x00" + d.Id + "\x00" + scope))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, hex.EncodeToString(lockKey[:])); err != nil {
		return err
	}
	var bindingID, assetID, previousHash string
	var previousTime time.Time
	err := tx.QueryRow(ctx, `SELECT id,asset_id,latest_fingerprint,inventory_at FROM asset_source_bindings WHERE source_instance_id=$1 AND external_type=$4 AND external_id=$2 AND scope=$3 FOR UPDATE`, sourceID, d.Id, scope, externalType).Scan(&bindingID, &assetID, &previousHash, &previousTime)
	creating := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !creating {
		return err
	}
	if !creating && observed.Before(previousTime) {
		_, err = tx.Exec(ctx, `UPDATE asset_source_bindings SET last_seen=now() WHERE id=$1`, bindingID)
		return err
	}
	raw, err := wireJSON(d)
	if err != nil {
		return err
	}
	fingerprint, err := assets.Fingerprint(d)
	if err != nil {
		return err
	}
	if len(provider) == 0 {
		provider = json.RawMessage(`{}`)
	}
	var object any
	decoder := json.NewDecoder(bytes.NewReader(provider))
	decoder.UseNumber()
	if err = decoder.Decode(&object); err != nil {
		return err
	}
	provider, err = json.Marshal(object)
	if err != nil {
		return err
	}
	if string(provider) != "{}" {
		hashProvider := provider
		if d.Source == "prometheus" {
			var obj map[string]any
			_ = json.Unmarshal(provider, &obj)
			delete(obj, "last_scrape")
			delete(obj, "source_revision")
			hashProvider, _ = json.Marshal(obj)
		}
		sum := sha256.Sum256(append([]byte(fingerprint+":"), hashProvider...))
		fingerprint = hex.EncodeToString(sum[:])
	}
	if creating {
		assetID, err = assetRegistryID("asset_")
		if err != nil {
			return err
		}
		bindingID, err = assetRegistryID("binding_")
		if err != nil {
			return err
		}
		assetScope := ""
		if scope != "" {
			assetScope = "simulated:" + sourceID + ":" + scope
		}
		name := d.Name
		if name == "" {
			name = d.Id
		}
		_, err = tx.Exec(ctx, `INSERT INTO assets(id,kind,canonical_name,vendor,model,serial,hardware_revision,software_version,firmware_version,scope) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, assetID, assets.Kind(d), name, d.Vendor, d.Model, d.SerialNumber, d.HardwareRevision, d.SoftwareVersion, d.FirmwareVersion, assetScope)
		if err != nil {
			return err
		}
		var legacyAgent, legacyDevice any
		if agentID != "" {
			legacyAgent = agentID
			legacyDevice = d.Id
		}
		_, err = tx.Exec(ctx, `INSERT INTO asset_source_bindings(id,asset_id,source_instance_id,external_type,external_id,provenance,scope,legacy_agent_id,legacy_device_id,inventory_at,inventory,latest_fingerprint) VALUES($1,$2,$3,$11,$4,$5,$6,$7,$12,$8,$9,$10)`, bindingID, assetID, sourceID, d.Id, provenance, scope, legacyAgent, observed, raw, fingerprint, externalType, legacyDevice)
		if err != nil {
			return err
		}
	}
	if creating || fingerprint != previousHash {
		var factID int64
		err = tx.QueryRow(ctx, `INSERT INTO asset_fact_versions(binding_id,observed_at,origin,origin_ref,fingerprint,inventory,provider_state) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`, bindingID, observed, origin, originRef, fingerprint, raw, provider).Scan(&factID)
		if err != nil {
			return err
		}
		for _, identity := range assets.Identities(d) {
			_, err = tx.Exec(ctx, `INSERT INTO asset_identity_evidence(binding_id,fact_id,kind,value,normalized_value,confidence,subject_id,usable,evidence) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, bindingID, factID, identity.Kind, identity.Value, identity.NormalizedValue, identity.Confidence, identity.SubjectID, identity.Usable, nullableJSON(identity.Evidence))
			if err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE asset_source_bindings SET latest_fact_id=$2 WHERE id=$1`, bindingID, factID); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE asset_source_bindings SET inventory_at=$2,inventory=$3,latest_fingerprint=$4,last_seen=now(),provider_state=$5 WHERE id=$1`, bindingID, observed, raw, fingerprint, provider); err != nil {
		return err
	}
	name := d.Name
	if name == "" {
		name = d.Id
	}
	_, err = tx.Exec(ctx, `UPDATE assets SET revision=revision+1,updated_at=now() WHERE id=$1`, assetID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE assets SET kind=$2,canonical_name=$3,vendor=$4,model=$5,serial=$6,hardware_revision=$7,software_version=$8,firmware_version=$9,canonical_binding_id=$10 WHERE id=$1 AND (canonical_binding_id IS NULL OR canonical_binding_id=$10)`, assetID, assets.Kind(d), name, d.Vendor, d.Model, d.SerialNumber, d.HardwareRevision, d.SoftwareVersion, d.FirmwareVersion, bindingID)
	if err != nil {
		return err
	}
	return refreshAssetResolution(ctx, tx, assetID)
}
