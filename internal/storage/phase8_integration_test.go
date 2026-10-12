package storage

import (
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"strings"
	"testing"
	"time"
)

func TestPhase8InventoryDiagnosticsAndConfigArchive(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	hello := registration("agent-1", now)
	hello.GetHello().ManagedConfigV1 = true
	hello.GetHello().RemoteManagementEnabled = true
	if err := s.Ingest(ctx, hello); err != nil {
		t.Fatal(err)
	}
	d := autoNetworkDevice("phase8-switch")
	d.IdentityEvidence[0].SubjectKind = "network_device"
	d.IdentityEvidence[0].SubjectId = d.Id
	d.IdentityEvidence[0].Normalization = "canonical"
	d.IdentityEvidence[0].Time = metadata(now, "snmp").Time
	d.ExtendedIdentityEvidence = []*pb.IdentityEvidence{{Type: "serial", Value: "REJECTED-SERIAL", Confidence: "high", Normalization: "rejected", SubjectKind: "network_device", Source: metadata(now, "snmp").Source}, {Type: "ip", Value: "192.0.2.1", Confidence: "low", SubjectId: "eth0", SubjectKind: "network_interface", Normalization: "canonical", Source: metadata(now, "snmp").Source}}
	if err := s.Ingest(ctx, assetInventory("agent-1", "session-1", "phase8-inventory", now, d)); err != nil {
		t.Fatal(err)
	}
	var usable bool
	if err := s.pool.QueryRow(ctx, `SELECT usable FROM asset_identity_evidence WHERE value='REJECTED-SERIAL'`).Scan(&usable); err != nil || usable {
		t.Fatal("diagnostic used for matching", usable, err)
	}
	var rootSubject string
	if err := s.pool.QueryRow(ctx, `SELECT subject_id FROM asset_identity_evidence WHERE value='ELTEX-SERIAL-1' AND confidence='high'`).Scan(&rootSubject); err != nil || rootSubject != "" {
		t.Fatal("owner scope lost", rootSubject, err)
	}
	device, err := s.Device(ctx, "agent-1", d.Id)
	if err != nil || !strings.Contains(string(device.Inventory), "extended_identity_evidence") || !strings.Contains(string(device.Inventory), "normalization") {
		t.Fatal("Phase8 inventory lost", err)
	}
	status := &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: "phase8-config-status", Sequence: 4, SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_ConfigStatus{ConfigStatus: &pb.ConfigApplyStatus{CollectorId: "agent-1", Revision: 1, ConfigurationDigest: strings.Repeat("a", 64), Status: pb.ConfigStatus_CONFIG_STATUS_REJECTED, ErrorCode: "not_supported"}}}
	if err := s.Ingest(ctx, status); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, status); err != nil {
		t.Fatal("status replay", err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM agent_messages WHERE message_id=$1 AND kind='config_status'`, status.MessageId).Scan(&count); err != nil || count != 1 {
		t.Fatal("status archive or dedup", count, err)
	}
}
