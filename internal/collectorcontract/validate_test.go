package collectorcontract

import (
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"
)

func fixture() *pb.AgentEnvelope {
	now := timestamppb.New(time.Now().UTC())
	return &pb.AgentEnvelope{AgentId: "collector-1", Payload: &pb.AgentEnvelope_Inventory{Inventory: &pb.InventoryUpdate{ObservedAt: now, Devices: []*pb.Device{{Id: "system", Hardware: &pb.HardwareInventory{ResourceType: "system", PowerState: "Off", Status: &pb.HardwareStatus{Health: "ok"}}, Observation: &pb.ObservationMetadata{Source: &pb.SourceRef{Type: "redfish", CollectorId: "collector-1", ResourceId: "/redfish/v1/Systems/1", Provenance: "real"}, Time: &pb.ObservationTime{ObservedAt: now, CollectedAt: now, ClockQuality: "unknown"}}}}}}}
}
func TestMetadataValidationAndLegacy(t *testing.T) {
	if err := Validate(&pb.AgentEnvelope{AgentId: "legacy"}); err != nil {
		t.Fatal(err)
	}
	if err := Validate(fixture()); err != nil {
		t.Fatal(err)
	} // Off is not Critical.
	for _, tc := range []struct {
		name   string
		change func(*pb.Device)
	}{
		{"collector_spoof", func(d *pb.Device) { d.Observation.Source.CollectorId = "another" }},
		{"missing_time", func(d *pb.Device) { d.Observation.Time = nil }},
		{"invalid_timestamp", func(d *pb.Device) { d.Observation.Time.SourceTimestamp = &timestamppb.Timestamp{Seconds: 253402300800} }},
		{"negative_uncertainty", func(d *pb.Device) { d.Observation.Time.Uncertainty = durationpb.New(-time.Second) }},
		{"overflow_offset", func(d *pb.Device) { d.Observation.Time.ClockOffset = &durationpb.Duration{Seconds: 315576000000} }},
		{"external_resource_url", func(d *pb.Device) { d.Observation.Source.ResourceId = "https://example.test/redfish" }},
		{"bad_health", func(d *pb.Device) { d.Hardware.Status.Health = "excellent" }},
		{"bad_identity", func(d *pb.Device) {
			d.IdentityEvidence = []*pb.IdentityEvidence{{Type: "serial", Value: "x", Confidence: "high", Source: &pb.SourceRef{Type: "redfish", CollectorId: "another"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := proto.Clone(fixture()).(*pb.AgentEnvelope)
			tc.change(e.GetInventory().Devices[0])
			if Validate(e) == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}
func TestSyslogTLSAttribution(t *testing.T) {
	e := &pb.AgentEnvelope{AgentId: "collector-1", Payload: &pb.AgentEnvelope_Logs{Logs: &pb.LogBatch{Records: []*pb.LogRecord{{Syslog: &pb.SyslogMetadata{Version: 1, Format: "rfc5424", ParseStatus: "parsed", PeerIp: "127.0.0.1", Transport: "tcp", Managed: true}}}}}}
	if err := Validate(e); err != nil {
		t.Fatal(err)
	}
	e.GetLogs().Records[0].Syslog.Authenticated = true
	if Validate(e) == nil {
		t.Fatal("TCP accepted as authenticated TLS")
	}
	e.GetLogs().Records[0].Syslog.Transport = "tls"
	e.GetLogs().Records[0].Syslog.TlsIdentity = "dns:host.example"
	if err := Validate(e); err != nil {
		t.Fatal(err)
	}
}
