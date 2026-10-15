package diagnosis

import (
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
)

func symptom(id, device, kind, resource, rule string, now time.Time) health.Incident {
	return health.Incident{ID: id, AgentID: "agent-1", DeviceID: device, ResourceKind: kind, ResourceID: resource, RuleID: rule, Source: "linux", State: "open", OpenedAt: now.Add(-time.Minute), LastObservedAt: now, EvidenceStatus: "fresh", Signal: health.Assessment{Status: "critical"}}
}
func TestContextUsesFreshPortMappingButNeverClaimsCausality(t *testing.T) {
	now := time.Now()
	seed := symptom("seed", "router", "interface", "port-1", "interface.oper_down", now)
	peer := symptom("peer", "switch", "device", "", "cpu.sustained", now)
	link := Link{EdgeID: "edge", AgentID: "agent-1", FromDevice: "router", ToDevice: "switch", LocalInterface: "port-1", Source: "lldp", Protocol: "DISCOVERY_PROTOCOL_LLDP", Confidence: "high", ObservedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute)}
	result := Build(now, seed, []health.Incident{peer}, []Link{link}, 300, 20, false)
	if len(result.Related) != 1 || result.Related[0].Relation != "port_and_neighbor_symptom_candidate" || result.Related[0].CausalClaim || len(result.Related[0].Links) != 1 {
		t.Fatal("mapped port context missing or cause invented")
	}
	link.ExpiresAt = now.Add(-time.Second)
	result = Build(now, seed, []health.Incident{peer}, []Link{link}, 300, 20, false)
	if len(result.Related[0].Links) != 0 || result.Related[0].Strength != "weak" {
		t.Fatal("expired topology used")
	}
	link.ExpiresAt = now.Add(time.Minute)
	link.Confidence = "medium"
	result = Build(now, seed, []health.Incident{peer}, []Link{link}, 300, 20, false)
	if len(result.Related[0].Links) != 0 {
		t.Fatal("hostname/weak identity used")
	}
}
func TestContextIsolationHistoryAndSIPNoInventedDependencies(t *testing.T) {
	now := time.Now()
	seed := symptom("seed", "router", "device", "", "cpu.sustained", now)
	other := symptom("other", "router", "device", "", "ram.sustained", now)
	other.AgentID = "different"
	demo := symptom("demo", "router", "device", "", "ram.sustained", now)
	demo.Source = "simulated"
	demo.Scope = "demo"
	sip := symptom("sip", "", "sip", "target", "sip.availability", now)
	result := Build(now, seed, []health.Incident{other, demo, sip}, nil, 300, 20, false)
	if len(result.Related) != 1 || result.Related[0].Relation != "same_collector_temporal_context" || result.Related[0].Strength != "weak" {
		t.Fatal("SIP/service dependency or identity invented")
	}
	seed.Source = "simulated"
	seed.Scope = "session-a"
	demo.Scope = "session-b"
	if len(Build(now, seed, []health.Incident{demo}, nil, 300, 20, false).Related) != 0 {
		t.Fatal("simulator sessions merged")
	}
	seed = symptom("seed", "router", "device", "", "cpu.sustained", now)
	seed.State = "resolved"
	resolved := now.Add(-time.Second)
	seed.ResolvedAt = &resolved
	if Build(now, seed, nil, nil, 300, 20, false).TopologyConsidered {
		t.Fatal("historical seed uses current topology")
	}
}
func TestCollectorContextTruncationAndTemporalBounds(t *testing.T) {
	now := time.Now()
	seed := symptom("seed", "", "agent", "agent-1", "collector.unavailable", now)
	peer := symptom("peer", "router", "device", "", "cpu.sustained", now)
	old := symptom("old", "router", "device", "", "ram.sustained", now)
	old.OpenedAt = now.Add(-time.Hour)
	end := now.Add(-30 * time.Minute)
	old.ResolvedAt = &end
	result := Build(now, seed, []health.Incident{peer, old}, nil, 300, 20, false)
	if len(result.Related) != 1 || result.Related[0].Relation != "collector_observability_context" {
		t.Fatal("collector/temporal context wrong")
	}
	second := peer
	second.ID = "peer-2"
	result = Build(now, seed, []health.Incident{peer, second}, nil, 300, 1, false)
	if !result.Truncated || len(result.Related) != 1 {
		t.Fatal("context not bounded")
	}
}
