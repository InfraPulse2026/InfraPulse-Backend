package storage

import (
	"encoding/json"
	"errors"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"strings"
	"testing"
	"time"
)

func metadata(now time.Time, kind string) *pb.ObservationMetadata {
	return &pb.ObservationMetadata{Source: &pb.SourceRef{Type: kind, CollectorId: "agent-1", TargetId: "fixture-target", Provenance: "real"}, Time: &pb.ObservationTime{ObservedAt: timestamppb.New(now), CollectedAt: timestamppb.New(now), ClockQuality: "unknown"}}
}
func TestCollectorHardwarePersistenceAndReplay(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	e := sampleEnvelope(now)
	v := e.GetTelemetry().Samples[0]
	v.Source = "redfish"
	v.Observation = metadata(now, "redfish")
	v.Metrics = []*pb.Metric{{Name: "temperature", Unit: "celsius", Value: 42, ResourceId: "/redfish/v1/Chassis/1/Sensors/1", ComponentId: "sensor-1", Status: &pb.HardwareStatus{Health: "warning"}, Observation: metadata(now, "redfish")}}
	v.HardwareHealth = []*pb.ComponentHealth{{Id: "fan-1", ResourceId: "/redfish/v1/Chassis/1", Status: &pb.HardwareStatus{Health: "critical"}, Observation: metadata(now, "redfish")}}
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	replay := proto.Clone(e).(*pb.AgentEnvelope)
	replay.Sequence = 90
	replay.SentAt = timestamppb.New(now.Add(time.Second))
	if err := s.Ingest(ctx, replay); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Metrics(ctx, "agent-1", "shared-router", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("replay: %v %d", err, len(rows))
	}
	var sample pb.TelemetrySample
	if err = protojson.Unmarshal(rows[0].Sample, &sample); err != nil {
		t.Fatal(err)
	}
	if len(sample.HardwareHealth) != 1 || sample.Metrics[0].Value != 42 || sample.Observation.Time.ClockOffset != nil {
		t.Fatal("hardware fields or unknown timing lost")
	}
	// A partial inventory must not delete an unrelated existing device.
	inv := &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: "hardware-inventory", Sequence: 3, SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_Inventory{Inventory: &pb.InventoryUpdate{ObservedAt: timestamppb.New(now), Completeness: pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_PARTIAL, Observation: metadata(now, "redfish"), Devices: []*pb.Device{{Id: "system", Source: "redfish", Hardware: &pb.HardwareInventory{ResourceType: "system", PowerState: "Off", Status: &pb.HardwareStatus{Health: "ok"}}, Observation: metadata(now, "redfish")}}}}}
	if err = s.Ingest(ctx, inv); err != nil {
		t.Fatal(err)
	}
	device, err := s.Device(ctx, "agent-1", "system")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(device.Inventory), `"power_state": "Off"`) && !strings.Contains(string(device.Inventory), `"power_state":"Off"`) {
		t.Fatal("power state lost")
	}
	if _, err = s.Device(ctx, "agent-1", "shared-router"); err != nil {
		t.Fatal("partial inventory deleted device")
	}
}
func TestCollectorSyslogProjectionAtomicityAndBusinessDedup(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	e := logEnvelope("syslog-1", now)
	r := e.GetLogs().Records[0]
	r.Source = "syslog"
	r.Observation = metadata(now, "syslog")
	r.Syslog = &pb.SyslogMetadata{Version: 1, Format: "rfc5424", ParseStatus: "parsed", Transport: "tcp", PeerIp: "127.0.0.1", StructuredData: `[auth password="private-syslog-secret"]`, RawPayload: []byte{0xff, 0xfe}, Managed: true}
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	repeat := proto.Clone(e).(*pb.AgentEnvelope)
	repeat.MessageId = "syslog-2"
	repeat.GetLogs().Records[0].Observation.Time.CollectedAt = timestamppb.New(now.Add(time.Second))
	repeat.GetLogs().Records[0].ObservedAt = timestamppb.New(now.Add(time.Second))
	if err := s.Ingest(ctx, repeat); err != nil {
		t.Fatal(err)
	}
	page, err := s.Logs(ctx, logbook.Filter{Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("business dedup: %v %d", err, len(page.Items))
	}
	record := page.Items[0]
	encoded, _ := json.Marshal(record)
	if !record.RawPayloadOmitted || record.Observation == nil || strings.Contains(string(encoded), "private-syslog-secret") || strings.Contains(string(record.Syslog), "//4=") {
		t.Fatal("metadata lost or secret/raw octets exposed")
	}
	changed := proto.Clone(e).(*pb.AgentEnvelope)
	changed.MessageId = "syslog-conflict"
	changed.GetLogs().Records[0].Syslog.PeerIp = "127.0.0.2"
	if err = s.Ingest(ctx, changed); !errors.Is(err, ErrLogConflict) {
		t.Fatalf("transport identity conflict: %v", err)
	}
	var n int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM agent_messages WHERE message_id='syslog-conflict'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("failed projection was committed")
	}
	bad := proto.Clone(e).(*pb.AgentEnvelope)
	bad.MessageId = "spoof"
	bad.GetLogs().Records[0].Observation.Source.CollectorId = "another"
	if s.Ingest(ctx, bad) == nil {
		t.Fatal("spoof accepted")
	}
}
