package assethardware

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func meta(at time.Time, protocol, target, resource string) *pb.ObservationMetadata {
	return &pb.ObservationMetadata{Source: &pb.SourceRef{Type: protocol, CollectorId: "collector", TargetId: target, Protocol: protocol, InstanceId: "driver-1", Provenance: "real", ResourceId: resource}, Time: &pb.ObservationTime{ObservedAt: timestamppb.New(at), CollectedAt: timestamppb.New(at), ClockQuality: "unknown"}}
}
func TestExtractPreservesZeroOffUnknownAndOrigin(t *testing.T) {
	at := time.Now()
	zero := uint64(0)
	d := &pb.Device{Id: "system", Name: "System", Source: "redfish", Observation: meta(at, "redfish", "target-1", "/Systems/1"), Hardware: &pb.HardwareInventory{ResourceType: "System", PowerState: "Off", Status: &pb.HardwareStatus{Health: "ok", HealthRollup: "warning"}, CapacityBytes: &zero}, Components: []*pb.InventoryComponent{{Id: "fan", Hardware: &pb.HardwareInventory{ResourceType: "Fan"}}}}
	e := &pb.AgentEnvelope{AgentId: "collector", SentAt: timestamppb.New(at), Payload: &pb.AgentEnvelope_Inventory{Inventory: &pb.InventoryUpdate{ObservedAt: timestamppb.New(at), Devices: []*pb.Device{d}}}}
	got, err := Extract(e)
	if err != nil || len(got) != 2 {
		t.Fatal(got, err)
	}
	if !strings.Contains(string(got[0].Payload), `"power_state":"Off"`) || !strings.Contains(string(got[0].Payload), `"health":"ok"`) || !strings.Contains(string(got[0].Payload), `"capacity_bytes":"0"`) {
		t.Fatal("Off/health/optional zero lost", string(got[0].Payload))
	}
	if strings.Contains(string(got[1].Payload), "capacity_bytes") {
		t.Fatal("missing capacity became zero")
	}
	sample := &pb.TelemetrySample{DeviceId: "system", ObservedAt: timestamppb.New(at), Source: "redfish", Observation: d.Observation, Metrics: []*pb.Metric{{Name: "hardware.fan.rpm", Value: 0, Unit: "RPM", ResourceId: "/Fans/1", ComponentId: "fan"}, {Name: "hardware.voltage.volts", Value: math.NaN(), Unit: "V"}, {Name: "system.cpu", Value: 10}}}
	e.Payload = &pb.AgentEnvelope_Telemetry{Telemetry: &pb.TelemetryBatch{Samples: []*pb.TelemetrySample{sample}}}
	got, err = Extract(e)
	if err != nil || len(got) != 2 {
		t.Fatal(got, err)
	}
	if got[0].Value == nil || *got[0].Value != 0 || got[1].Value != nil || got[1].DataQuality != "non_finite" {
		t.Fatal("numeric semantics lost", got)
	}
	if !CompatibleSource(got[0], d) {
		t.Fatal("own source rejected")
	}
	foreign := proto.Clone(d).(*pb.Device)
	foreign.Observation.Source.TargetId = "target-2"
	if CompatibleSource(got[0], foreign) {
		t.Fatal("foreign target accepted")
	}
	changed := got[0]
	var ref map[string]any
	_ = json.Unmarshal(changed.SourceRef, &ref)
	ref["type"] = "ipmi"
	changed.SourceRef, _ = json.Marshal(ref)
	changed.namespace = "ipmi"
	if changed.Key("binding") == got[0].Key("binding") || got[0].Key("binding") == got[0].Key("another-binding") {
		t.Fatal("protocol/binding namespace collapsed")
	}
}
func TestFreshnessAndBounds(t *testing.T) {
	now := time.Now()
	o := Observation{ObservedAt: now, DataQuality: "valid"}
	if Freshness(o, now, now, 300, true, true) != "fresh" {
		t.Fatal("fresh missing")
	}
	o.ObservedAt = now.Add(-301 * time.Second)
	if Freshness(o, now, now, 300, true, true) != "stale" {
		t.Fatal("stale missing")
	}
	o.ObservedAt = now.Add(time.Minute)
	if Freshness(o, now, now, 300, true, true) != "clock_skew" {
		t.Fatal("skew missing")
	}
	o.ObservedAt = now
	o.ClockQuality = "unsynced"
	if Freshness(o, now, now, 300, true, true) != "clock_degraded" {
		t.Fatal("degraded missing")
	}
	o.ClockQuality = ""
	if Freshness(o, now, now, 300, true, false) != "source_unavailable" {
		t.Fatal("offline fresh")
	}
	old := now.Add(-time.Hour)
	o.SourceTimestamp = &old
	if Freshness(o, now, now, 300, true, true) != "source_stale" {
		t.Fatal("stale source reading refreshed by collection")
	}
	future := now.Add(time.Hour)
	o.SourceTimestamp = &future
	if Freshness(o, now, now, 300, true, true) != "clock_skew" {
		t.Fatal("future source reading treated fresh")
	}
	for _, f := range []Filter{{Limit: 201}, {Offset: -1}, {Kind: "power"}, {MaxAgeSeconds: 86401}} {
		if _, err := Defaults(f); err == nil {
			t.Fatal("bad filter accepted", f)
		}
	}
	if _, err := HistoryDefaults(HistoryFilter{ObservationKey: strings.Repeat("z", 64)}); err == nil {
		t.Fatal("invalid cursor accepted")
	}
}
