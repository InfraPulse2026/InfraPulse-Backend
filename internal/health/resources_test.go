package health

import (
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/proto"
)

func portFixture() *pb.Interface {
	speed := uint64(8_000_000)
	return &pb.Interface{Id: "port-1", AdminUp: true, OperState: "up", Metadata: &pb.InterfaceMetadata{AdminStatus: true}, SpeedBitsPerSecond: &speed, Rates: &pb.InterfaceRates{RxBytesPerSecond: 800_000, TxBytesPerSecond: 800_000, RxErrorsPerSecond: 2, TxDroppedPerSecond: 20}}
}
func portSeries(now time.Time, port *pb.Interface) []ResourceSample {
	items := []ResourceSample{}
	for i := 0; i < 3; i++ {
		items = append(items, ResourceSample{ID: int64(i + 1), ObservedAt: now.Add(time.Duration(i-2) * 3 * time.Second), ReceivedAt: now, BootID: "boot-1", Interface: proto.Clone(port).(*pb.Interface)})
	}
	return items
}
func TestInterfaceDuplexUtilizationAndMeasuredRates(t *testing.T) {
	now := time.Now()
	rules := InterfaceRules(now, portSeries(now, portFixture()), 3)
	if rules[1].Status != "warning" || rules[1].NumericValue == nil || *rules[1].NumericValue != 80 {
		t.Fatal("full duplex directions were added or utilization missing")
	}
	if rules[2].Status != "warning" || rules[2].Unit != "per_second" || rules[2].Value != nil {
		t.Fatal("error rate converted to percent")
	}
	if rules[3].Status != "warning" {
		t.Fatal("drop rate not detected")
	}
}
func TestInterfaceUnknownAdminAndCountersAreNotRates(t *testing.T) {
	now := time.Now()
	port := portFixture()
	port.Rates = nil
	port.Counters = &pb.InterfaceCounters{RxErrors: 999999}
	if rule := InterfaceRules(now, portSeries(now, port), 3)[2]; rule.Status != "unknown" {
		t.Fatal("cumulative counters interpreted as rates")
	}
	port = portFixture()
	port.Rates.Available = &pb.CounterAvailability{RxBytes: true, TxBytes: true}
	if rule := InterfaceRules(now, portSeries(now, port), 3)[2]; rule.Status != "unknown" {
		t.Fatal("unavailable rate interpreted as zero")
	}
	port = portFixture()
	port.OperState = "down"
	port.AdminUp = false
	if rule := InterfaceRules(now, portSeries(now, port), 3)[0]; rule.Status != "unknown" || rule.RecoveryEligible {
		t.Fatal("administrative shutdown called outage/recovery")
	}
	port.AdminUp = true
	if rule := InterfaceRules(now, portSeries(now, port), 3)[0]; rule.Status != "critical" {
		t.Fatal("sustained oper_down not detected")
	}
	port.Metadata = nil
	if rule := InterfaceRules(now, portSeries(now, port), 3)[0]; rule.Status != "unknown" {
		t.Fatal("unknown administrative state inferred")
	}
}
func sipSeries(now time.Time, state string, latency *float64) []ResourceSample {
	items := []ResourceSample{}
	for i := 0; i < 3; i++ {
		h := &pb.TelephonyHealth{State: state, LatencyMs: latency, Address: "sip:fixture", Transport: "udp"}
		if state != "down" {
			h.Reachable = true
			code := uint32(200)
			if state == "degraded" {
				code = 401
			}
			h.ResponseCode = &code
		}
		items = append(items, ResourceSample{ID: int64(i + 1), ObservedAt: now.Add(time.Duration(i-2) * 3 * time.Second), ReceivedAt: now, Identity: SIPContext(h.Address, h.Transport), SIP: h})
	}
	return items
}
func TestSIPFailureLatencyAndEndpointIsolation(t *testing.T) {
	now := time.Now()
	down := SIPRules(now, sipSeries(now, "down", nil), 3)
	if down[0].Status != "critical" || down[1].Status != "unknown" {
		t.Fatal("SIP failure missing or absent latency zeroed")
	}
	high := 500.0
	degraded := SIPRules(now, sipSeries(now, "degraded", &high), 3)
	if degraded[0].Status != "warning" || degraded[1].Status != "warning" || degraded[1].Unit != "milliseconds" || degraded[1].Value != nil {
		t.Fatal("SIP degraded/latency semantics wrong")
	}
	low := 50.0
	up := SIPRules(now, sipSeries(now, "up", &low), 3)
	if !up[0].RecoveryEligible || !up[1].RecoveryEligible {
		t.Fatal("stable SIP recovery missing")
	}
	points := sipSeries(now, "down", nil)
	points[2].Identity = SIPContext("sip:another", "udp")
	if rule := SIPRules(now, points, 3)[0]; rule.Status != "pending" {
		t.Fatal("different endpoints pooled")
	}
	if rule := SIPRules(now.Add(12*time.Second), sipSeries(now, "up", &low), 3)[0]; rule.Status != "stale" || rule.RecoveryEligible {
		t.Fatal("stale SIP recovered")
	}
}

func TestResourceSpikeFutureAndMissingSpeedDoNotInventOutages(t *testing.T) {
	now := time.Now()
	points := sipSeries(now, "up", nil)
	points[2].SIP.State = "down"
	points[2].SIP.Reachable = false
	points[2].SIP.ResponseCode = nil
	points[2].SIP.ConsecutiveFailures = 99
	if a := SIPRules(now, points, 3)[0]; a.Status != "pending" {
		t.Fatal("one SIP observation or advertised failure count created confirmed incident")
	}
	port := portFixture()
	port.SpeedBitsPerSecond = nil
	if a := InterfaceRules(now, portSeries(now, port), 3)[1]; a.Status != "unknown" {
		t.Fatal("unknown speed yielded utilization")
	}
	for i := range points {
		points[i].ObservedAt = now.Add(time.Minute)
	}
	if a := SIPRules(now, points, 3)[0]; a.Status != "unknown" {
		t.Fatal("future SIP triggered incident")
	}
}
