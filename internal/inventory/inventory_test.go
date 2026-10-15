package inventory

import (
	"encoding/json"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"google.golang.org/protobuf/types/known/timestamppb"
	"strings"
	"testing"
	"time"
)

func TestInventoryRootEvidenceConflictsAndProvenance(t *testing.T) {
	now := time.Now().UTC()
	d := &pb.Device{Id: "system", Name: "Friendly name", Hostname: "hostname-is-not-a-serial", Vendor: "Eltex", Model: "Model-1", SerialNumber: "UNKNOWN", Source: "redfish", Type: pb.DeviceType_DEVICE_TYPE_SERVER, IdentityEvidence: []*pb.IdentityEvidence{{Type: "serial", Value: "REAL-123", Confidence: "high", Time: &pb.ObservationTime{ObservedAt: timestamppb.New(now.Add(-time.Minute)), ClockQuality: "synced"}}, {Type: "serial", Value: "DRIVE-999", Confidence: "high", SubjectId: "drive"}, {Type: "serial", Value: "INVALID-123", Confidence: "high", Normalization: "rejected"}}, Components: []*pb.InventoryComponent{{Id: "drive", SerialNumber: "DRIVE-999"}}}
	out := Build(assets.Asset{ID: "asset", CanonicalBindingID: "a", Revision: 5}, []Snapshot{{BindingID: "a", SourceID: "src-a", SourceType: "COLLECTOR", Provenance: "real", FactID: 42, ObservedAt: now, ReceivedAt: now, Device: d}, {BindingID: "b", SourceID: "src-b", SourceType: "ZABBIX", Device: &pb.Device{SerialNumber: "WRONG-123"}}})
	serial := out.Attributes["serial"]
	if !serial.Conflict || serial.Selected == nil || string(serial.Selected.Value) != `"REAL-123"` || serial.Selected.SourceID != "src-a" || serial.Selected.FactID != 42 || !serial.Selected.ObservedAt.Equal(now.Add(-time.Minute)) || serial.Selected.CollectorProtocol != "redfish" {
		t.Fatal("verified root evidence or conflict lost", serial)
	}
	for _, c := range serial.Claims {
		if string(c.Value) == `"DRIVE-999"` {
			t.Fatal("component became system serial")
		}
		if string(c.Value) == `"INVALID-123"` && c.Usable {
			t.Fatal("rejected evidence used")
		}
	}
	out = Build(assets.Asset{}, []Snapshot{{Device: &pb.Device{Hostname: "serial-looking-hostname"}}})
	if out.Attributes["serial"].Selected != nil || out.Status != "partial" {
		t.Fatal("hostname guessed into serial", out)
	}
}
func TestInventoryEqualConfidenceDoesNotHideConflictAndSetsAreScoped(t *testing.T) {
	snapshots := []Snapshot{{BindingID: "canonical", Device: &pb.Device{Vendor: "Vendor", ManagementAddress: "::ffff:10.0.0.1", IdentityEvidence: []*pb.IdentityEvidence{{Type: "serial", Value: "ROOT-A", Confidence: "high"}}}}, {BindingID: "other", Device: &pb.Device{Vendor: "vendor", ManagementAddress: "10.0.0.2", IdentityEvidence: []*pb.IdentityEvidence{{Type: "serial", Value: "ROOT-B", Confidence: "high"}}}}}
	out := Build(assets.Asset{CanonicalBindingID: "canonical"}, snapshots)
	if out.Attributes["serial"].Selected != nil || out.Attributes["serial"].Status != "conflict" || out.Attributes["vendor"].Conflict || out.Attributes["vendor"].Selected.BindingID != "canonical" {
		t.Fatal("conflict hidden by canonical source", out)
	}
	if out.Attributes["management_ips"].Conflict || len(out.Attributes["management_ips"].Claims) != 2 {
		t.Fatal("addresses merged without provenance")
	}
	var ips []string
	_ = json.Unmarshal(out.Attributes["management_ips"].Claims[0].Value, &ips)
	if len(ips) != 1 || ips[0] != "10.0.0.1" {
		t.Fatal("IP normalization", ips)
	}
	out = Build(assets.Asset{}, nil)
	if out.Status != "unknown" || len(out.Missing) != 3 || out.Attributes["serial"].Claims == nil {
		t.Fatal("missing inventory invented", out)
	}
}

func TestInventoryZabbixAllManagementInterfacesAndRejectedField(t *testing.T) {
	out := Build(assets.Asset{}, []Snapshot{{SourceType: "ZABBIX", BindingID: "zab", ProviderState: json.RawMessage(`{"credential":"do-not-expose","interfaces":[{"interfaceid":"1","useip":"1","ip":"10.0.0.1","port":"161"},{"interfaceid":"2","useip":"1","ip":"10.0.0.2","details":{"password":"do-not-expose"}}]}`), Device: &pb.Device{SerialNumber: "REJECTED-1", IdentityEvidence: []*pb.IdentityEvidence{{Type: "serial", Value: "REJECTED-1", Confidence: "high", Normalization: "rejected"}}}}})
	if out.Attributes["serial"].Selected != nil {
		t.Fatal("rejected identity reintroduced via legacy field")
	}
	if string(out.Attributes["management_ips"].Value) != `["10.0.0.1","10.0.0.2"]` || out.Attributes["management_interfaces"].Status != "source_scoped" {
		t.Fatal("interface inventory incomplete", out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "do-not-expose") {
		t.Fatal("arbitrary provider fields leaked")
	}
}
