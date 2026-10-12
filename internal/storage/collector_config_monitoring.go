package storage

import (
	"context"
	"encoding/json"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"math"
	"strings"
	"time"
)

func managedSeconds(v time.Duration, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return int(math.Ceil(v.Seconds()))
}

// Derived intervals use the last successfully applied snapshot. Desired,
// rejected and uncorrelated revisions never alter freshness expectations.
func (s *Store) refreshManagedMonitoring(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope) error {
	if e.GetInventory() == nil && e.GetHello() == nil && e.GetConfigStatus() == nil && e.GetTelephony() == nil && (e.GetHeartbeat() == nil || e.GetHeartbeat().ManagedConfig == nil) {
		return nil
	}

	var raw []byte
	var revision int64
	onlyTransition := e.GetInventory() == nil && e.GetTelephony() == nil
	err := tx.QueryRow(ctx, `SELECT r.snapshot,r.revision FROM collector_config_state c JOIN collector_config_revisions r ON r.collector_id=c.collector_id AND r.revision=c.applied_revision AND r.digest=c.applied_digest WHERE c.collector_id=$1 AND (NOT $2 OR c.monitoring_revision<>c.applied_revision)`, e.AgentId, onlyTransition).Scan(&raw, &revision)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	snap := &pb.DesiredConfigSnapshot{}
	if err = protojson.Unmarshal(raw, snap); err != nil {
		return err
	}
	type policy struct {
		Target    string `json:"target"`
		RawTarget string `json:"raw_target"`
		Protocol  string `json:"protocol"`
		Fast      int    `json:"fast"`
		Inventory int    `json:"inventory"`
		Topology  int    `json:"topology"`
	}
	policies := []policy{}
	for _, target := range snap.Targets {
		if !target.Enabled {
			continue
		}
		p := target.GetPolling()
		fast := p.GetFastInterval().AsDuration()
		fallback := s.defaults.TelemetryIntervalSeconds
		if target.Protocol == "sip" {
			fallback = s.telephonyDefault()
		}
		if target.Protocol == "redfish" || target.Protocol == "ipmi" {
			fast = p.GetMediumInterval().AsDuration()
		}
		policies = append(policies, policy{"remote:" + target.TargetId, target.TargetId, strings.ToLower(target.Protocol), managedSeconds(fast, fallback), managedSeconds(p.GetInventoryInterval().AsDuration(), s.defaults.InventoryIntervalSeconds), managedSeconds(p.GetTopologyInterval().AsDuration(), s.defaults.TopologyIntervalSeconds)})
	}
	payload, err := json.Marshal(policies)
	if err != nil {
		return err
	}
	// Fixed query count regardless of target count; don't rewrite unchanged rows
	// on every heartbeat or sample. Explicit operator overrides are preserved.
	for _, table := range []string{"monitoring_policies", "telephony_policies"} {
		if _, err = tx.Exec(ctx, `DELETE FROM `+table+` WHERE agent_id=$1 AND origin='collector_applied' AND managed_revision<>$2`, e.AgentId, revision); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO telephony_policies(agent_id,target_id,expected_interval_seconds,origin,managed_revision)
 SELECT t.agent_id,t.target_id,p.fast,'collector_applied',$3 FROM jsonb_to_recordset($2::jsonb) AS p(target text,raw_target text,protocol text,fast integer,inventory integer,topology integer)
 JOIN telephony_targets t ON t.agent_id=$1 AND t.target_id=p.raw_target JOIN telephony_samples v ON v.id=t.latest_sample_id JOIN agent_messages m ON m.agent_id=v.agent_id AND m.message_id=v.message_id
 WHERE p.protocol='sip' AND EXISTS(SELECT 1 FROM jsonb_array_elements(COALESCE(m.payload->'telephony'->'targets','[]'::jsonb)) h WHERE h->>'target_id'=t.target_id AND h->'observation'->'source'->>'collector_id'=$1 AND h->'observation'->'source'->>'target_id'=p.target AND h->'observation'->'source'->>'protocol'='sip' AND h->'observation'->'source'->>'provenance'='real')
 ON CONFLICT(agent_id,target_id) DO UPDATE SET expected_interval_seconds=EXCLUDED.expected_interval_seconds,managed_revision=EXCLUDED.managed_revision,updated_at=now() WHERE telephony_policies.origin='collector_applied' AND (telephony_policies.managed_revision IS DISTINCT FROM EXCLUDED.managed_revision OR telephony_policies.expected_interval_seconds<>EXCLUDED.expected_interval_seconds)`, e.AgentId, payload, revision)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO monitoring_policies(agent_id,device_id,telemetry_interval_seconds,inventory_interval_seconds,topology_interval_seconds,origin,managed_revision)
 SELECT d.agent_id,d.device_id,p.fast,p.inventory,p.topology,'collector_applied',$3 FROM jsonb_to_recordset($2::jsonb) AS p(target text,raw_target text,protocol text,fast integer,inventory integer,topology integer)
 JOIN devices d ON d.agent_id=$1 AND d.inventory->'observation'->'source'->>'collector_id'=$1 AND d.inventory->'observation'->'source'->>'target_id'=p.target AND d.inventory->'observation'->'source'->>'protocol'=p.protocol AND d.inventory->'observation'->'source'->>'provenance'='real'
 WHERE p.protocol<>'sip'
 ON CONFLICT(agent_id,device_id) DO UPDATE SET telemetry_interval_seconds=EXCLUDED.telemetry_interval_seconds,inventory_interval_seconds=EXCLUDED.inventory_interval_seconds,topology_interval_seconds=EXCLUDED.topology_interval_seconds,managed_revision=EXCLUDED.managed_revision,updated_at=now() WHERE monitoring_policies.origin='collector_applied' AND (monitoring_policies.managed_revision IS DISTINCT FROM EXCLUDED.managed_revision OR (monitoring_policies.telemetry_interval_seconds,monitoring_policies.inventory_interval_seconds,monitoring_policies.topology_interval_seconds) IS DISTINCT FROM (EXCLUDED.telemetry_interval_seconds,EXCLUDED.inventory_interval_seconds,EXCLUDED.topology_interval_seconds))`, e.AgentId, payload, revision)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE collector_config_state SET monitoring_revision=$2 WHERE collector_id=$1`, e.AgentId, revision)
	return err
}
