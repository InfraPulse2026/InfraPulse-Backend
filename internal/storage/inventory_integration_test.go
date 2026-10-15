package storage

import (
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

func TestResolvedInventoryCurrentFactsHistoryConflictAndDisabledSource(t *testing.T) {
	s, ctx, items, bindings, d := assetDecisionFixture(t)
	if _, _, err := s.MergeAssets(ctx, assetMergeInput(items, bindings), "inventory-merge"); err != nil {
		t.Fatal(err)
	}
	out, err := s.ResolvedInventory(ctx, items[0].ID)
	if err != nil || out.SourceCount != 2 || out.Attributes["serial"].Selected == nil {
		t.Fatal("combined inventory", out, err)
	}
	initial := out.Attributes["serial"].Selected.FactID
	same := proto.Clone(d).(*pb.Device)
	same.UptimeSeconds = 42
	if err = s.Ingest(ctx, assetInventory(*bindings[0].LegacyAgentID, "session-1", "new-clock", time.Now(), same)); err != nil {
		t.Fatal(err)
	}
	out, err = s.ResolvedInventory(ctx, items[0].ID)
	if err != nil || out.Attributes["serial"].Selected.FactID != initial {
		t.Fatal("refresh invented fact version", err)
	}
	conflict := proto.Clone(d).(*pb.Device)
	conflict.SerialNumber = "ROOT-DIFFERENT"
	conflict.IdentityEvidence[1].Value = conflict.SerialNumber
	if err = s.Ingest(ctx, assetInventory(*bindings[1].LegacyAgentID, "session-1", "conflicting-fact", time.Now().Add(time.Second), conflict)); err != nil {
		t.Fatal(err)
	}
	out, err = s.ResolvedInventory(ctx, items[0].ID)
	if err != nil || out.Attributes["serial"].Selected != nil || !out.Attributes["serial"].Conflict || out.Status != "conflict" {
		t.Fatal("conflict lost", out, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE data_sources SET enabled=false WHERE id=$1`, bindings[1].SourceInstanceID); err != nil {
		t.Fatal(err)
	}
	out, err = s.ResolvedInventory(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	for _, c := range out.Attributes["serial"].Claims {
		if c.SourceID == bindings[1].SourceInstanceID {
			disabled = !c.SourceEnabled
		}
	}
	if !disabled {
		t.Fatal("disabled source state hidden")
	}
	facts, err := s.AssetFacts(ctx, items[0].ID, 50, 0)
	if err != nil || len(facts) != 3 {
		t.Fatal("fact chronology lost", facts, err)
	}
	alias, err := s.Asset(ctx, items[1].ID)
	if err != nil || alias.LifecycleStatus != "merged" {
		t.Fatal("asset alias lost", err)
	}
	// Preserve existing source-owned cards; consolidated view is additive.
	legacy, err := s.Device(ctx, *bindings[1].LegacyAgentID, "system")
	if err != nil || legacy.DeviceID != "system" {
		t.Fatal("legacy API broken", err)
	}
}
