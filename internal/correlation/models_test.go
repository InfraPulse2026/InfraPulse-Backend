package correlation

import (
	"testing"
	"time"
)

func testSignal(id string, at time.Time) Signal {
	return Signal{ID: id, Status: "active", Category: "network", SourceKind: "snmp", TimeQuality: "exact", ObservedAt: at, AssetIDs: []string{"asset-1"}, ServiceIDs: []string{}, SiteIDs: []string{}}
}

func TestScoreRequiresStructuralEvidence(t *testing.T) {
	now := time.Now().UTC()
	a := testSignal("a", now)
	b := testSignal("b", now.Add(time.Second))
	b.AssetIDs = []string{"asset-2"}
	b.SourceKind = "zabbix"
	d := Score(a, b, false)
	if d.Eligible || d.Score >= Threshold {
		t.Fatalf("time and source diversity must not group unrelated assets: %+v", d)
	}
}

func TestScoreSameAssetAndSourceDiversity(t *testing.T) {
	now := time.Now().UTC()
	a := testSignal("a", now)
	b := testSignal("b", now.Add(10*time.Second))
	b.SourceKind = "zabbix"
	d := Score(a, b, false)
	if !d.Eligible || d.Score < 70 || d.Relation != "same_asset" || d.CausalClaim {
		t.Fatalf("expected explainable same-asset grouping without causal claim: %+v", d)
	}
}

func TestScoreTopologyAndTime(t *testing.T) {
	now := time.Now().UTC()
	a := testSignal("a", now)
	b := testSignal("b", now.Add(10*time.Second))
	b.AssetIDs = []string{"asset-2"}
	b.SiteIDs = []string{"site-1"}
	a.SiteIDs = []string{"site-1"}
	b.SourceKind = "sip.options"
	d := Score(a, b, true)
	if !d.Eligible || d.Relation != "topology_neighbor" {
		t.Fatalf("expected topology-backed grouping: %+v", d)
	}
}

func TestScoreLowQualityTimeLowersConfidence(t *testing.T) {
	now := time.Now().UTC()
	a := testSignal("a", now)
	b := testSignal("b", now)
	b.SourceKind = "zabbix"
	b.TimeQuality = "received_only"
	d := Score(a, b, false)
	if !d.Eligible || d.TimeConfidence != "low" || d.Confidence != "low" {
		t.Fatalf("same Asset may group, but bad time must cap confidence: %+v", d)
	}
}

func TestServiceInputIsClosedAndRevisioned(t *testing.T) {
	valid := ServiceInput{Name: "Voice branch", Kind: "voice", Enabled: true, Assets: []AssetRole{{AssetID: "asset-1", Role: "provider"}}, Confirmed: true, Reason: "reviewed dependency"}
	if err := valid.Validate(true); err != nil {
		t.Fatal(err)
	}
	duplicate := valid
	duplicate.Assets = append(duplicate.Assets, duplicate.Assets[0])
	if duplicate.Validate(true) == nil {
		t.Fatal("duplicate Asset must be rejected")
	}
	update := valid
	update.ExpectedRevision = 1
	if err := update.Validate(false); err != nil {
		t.Fatal(err)
	}
}

func TestScoreExcludesStaleWindowResolvedAndSimulationCollision(t *testing.T) {
	now := time.Now().UTC()
	a := testSignal("a", now)
	b := testSignal("b", now.Add(-time.Hour))
	if Score(a, b, true).Eligible {
		t.Fatal("unrelated time window grouped")
	}
	b.ObservedAt = now
	b.Status = "resolved"
	if Score(a, b, true).Eligible {
		t.Fatal("resolved signal grouped as active")
	}
	b.Status = "active"
	a.Scope = "session"
	b.Scope = "session"
	a.Payload = map[string]any{"agent_id": "collector-a"}
	b.Payload = map[string]any{"agent_id": "collector-b"}
	if Score(a, b, true).Eligible {
		t.Fatal("simulation session collision across collectors")
	}
}

func TestScoreMultipleCollectorsReviewedService(t *testing.T) {
	now := time.Now().UTC()
	a, b := testSignal("a", now), testSignal("b", now)
	b.AssetIDs = []string{"asset-2"}
	a.ServiceIDs = []string{"service-1"}
	b.ServiceIDs = []string{"service-1"}
	one, two := "collector-1", "collector-2"
	a.SourceInstanceID = &one
	b.SourceInstanceID = &two
	if d := Score(a, b, false); !d.Eligible || d.Relation != "same_service" {
		t.Fatalf("reviewed service across collectors: %+v", d)
	}
}
