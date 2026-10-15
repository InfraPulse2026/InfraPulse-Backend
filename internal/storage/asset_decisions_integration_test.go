package storage

import (
	"context"
	"sync"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"google.golang.org/protobuf/proto"
)

func assetDecisionFixture(t *testing.T) (*Store, context.Context, []assets.Asset, []assets.Binding, *pb.Device) {
	t.Helper()
	s := integrationStore(t)
	_, admin := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), admin)
	now := time.Now()
	d := &pb.Device{Id: "system", Name: "System", Vendor: "Eltex", SerialNumber: "ROOT-1", Source: "redfish", Hardware: &pb.HardwareInventory{ResourceType: "ComputerSystem", HardwareUuid: "abcdef12-1234-1234-1234-123456789abc"}, IdentityEvidence: []*pb.IdentityEvidence{{Type: "hardware_uuid", Value: "abcdef12-1234-1234-1234-123456789abc", Confidence: "high", Source: &pb.SourceRef{Type: "redfish", Provenance: "real"}}, {Type: "serial", Value: "ROOT-1", Confidence: "high", Source: &pb.SourceRef{Type: "redfish", Provenance: "real"}}}}
	for _, agent := range []string{"agent-a", "agent-b"} {
		if err := s.Ingest(ctx, registration(agent, now)); err != nil {
			t.Fatal(err)
		}
		if err := s.Ingest(ctx, assetInventory(agent, "session-1", "system", now, d)); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.Assets(ctx, "system", "real", 50, 0)
	if err != nil || len(items) != 2 {
		t.Fatal(items, err)
	}
	bindings := []assets.Binding{}
	for _, a := range items {
		b, e := s.AssetBindings(ctx, a.ID, true, 50, 0)
		if e != nil || len(b) != 1 {
			t.Fatal(e)
		}
		bindings = append(bindings, b[0])
	}
	return s, ctx, items, bindings, d
}
func assetMergeInput(items []assets.Asset, bindings []assets.Binding) assets.MergeInput {
	return assets.MergeInput{TargetID: items[0].ID, TargetRevision: items[0].Revision, SourceID: items[1].ID, SourceRevision: items[1].Revision, CanonicalBindingID: bindings[0].ID, Confirmed: true, Reason: "checked against rack labels"}
}

func TestAssetManualMergeSplitHistoryAndCanonicalOwnership(t *testing.T) {
	s, ctx, items, bindings, d := assetDecisionFixture(t)
	candidates, err := s.AssetCandidates(ctx, items[0].ID, 20)
	if err != nil || len(candidates) != 1 || !candidates[0].Comparison.StrongMatch {
		t.Fatal("matching explanation", candidates, err)
	}
	in := assetMergeInput(items, bindings)
	decision, replay, err := s.MergeAssets(ctx, in, "merge-one")
	if err != nil || replay {
		t.Fatal("merge", err)
	}
	prior, replay, err := s.MergeAssets(ctx, in, "merge-one")
	if err != nil || !replay || prior.ID != decision.ID {
		t.Fatal("merge replay", err)
	}
	if _, _, err = s.MergeAssets(ctx, in, "different-merge"); err != assets.ErrConflict {
		t.Fatal("stale merge applied", err)
	}
	a, err := s.Asset(ctx, items[0].ID)
	if err != nil || a.SourceCount != 2 || a.ResolutionStatus != "resolved" {
		t.Fatal(a, err)
	}
	listed, e := s.Assets(ctx, "system", "real", 50, 0)
	if e != nil || len(listed) != 1 {
		t.Fatal("merged alias duplicated active cards", listed, e)
	}
	alias, err := s.Asset(ctx, items[1].ID)
	if err != nil || alias.LifecycleStatus != "merged" || alias.MergedIntoID == nil || *alias.MergedIntoID != a.ID {
		t.Fatal("alias lost", alias, err)
	}
	update := proto.Clone(d).(*pb.Device)
	update.Name = "noncanonical name"
	if err = s.Ingest(ctx, assetInventory(*bindings[1].LegacyAgentID, "session-1", "after-merge", time.Now().Add(time.Second), update)); err != nil {
		t.Fatal(err)
	}
	a, err = s.Asset(ctx, a.ID)
	if err != nil || a.CanonicalName != "System" || a.CanonicalBindingID != bindings[0].ID {
		t.Fatal("incoming source replaced canonical", a, err)
	}
	split := assets.SplitInput{AssetID: a.ID, Revision: a.Revision, BindingIDs: []string{bindings[1].ID}, CanonicalBindingID: bindings[1].ID, Confirmed: true, Reason: "separate for verification"}
	separation, replay, err := s.SplitAsset(ctx, split, "split-one")
	if err != nil || replay {
		t.Fatal("split", err)
	}
	if _, replay, err = s.SplitAsset(ctx, split, "split-one"); err != nil || !replay {
		t.Fatal("split replay", err)
	}
	newAsset, err := s.Asset(ctx, separation.TargetID)
	if err != nil || newAsset.SourceCount != 1 {
		t.Fatal("split destination", newAsset, err)
	}
	oldAsset, err := s.Asset(ctx, a.ID)
	if err != nil || oldAsset.SourceCount != 1 {
		t.Fatal("split removed all bindings", oldAsset, err)
	}
	candidates, err = s.AssetCandidates(ctx, oldAsset.ID, 20)
	if err != nil || len(candidates) != 1 || !candidates[0].BlockedByDecision {
		t.Fatal("split not remembered", candidates, err)
	}
	history, err := s.AssetDecisions(ctx, oldAsset.ID, 50, 0)
	if err != nil || len(history) != 2 {
		t.Fatal("decision history lost", history, err)
	}
	facts, err := s.AssetFacts(ctx, newAsset.ID, 50, 0)
	if err != nil || len(facts) < 1 {
		t.Fatal("split lost fact history", err)
	}
}

func TestAssetMergeVetoAndConflictAfterIdentityChange(t *testing.T) {
	s, ctx, items, bindings, d := assetDecisionFixture(t)
	changed := proto.Clone(d).(*pb.Device)
	changed.SerialNumber = "ROOT-2"
	changed.IdentityEvidence[1].Value = "ROOT-2"
	if err := s.Ingest(ctx, assetInventory(*bindings[1].LegacyAgentID, "session-1", "contradiction", time.Now().Add(time.Second), changed)); err != nil {
		t.Fatal(err)
	}
	in := assetMergeInput(items, bindings)
	fresh, err := s.Asset(ctx, items[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	in.SourceRevision = fresh.Revision
	if _, _, err = s.MergeAssets(ctx, in, "conflicting-merge"); err != assets.ErrConflict {
		t.Fatal("serial contradiction ignored", err)
	}
	candidates, err := s.AssetCandidates(ctx, items[0].ID, 20)
	if err != nil || len(candidates) != 1 || candidates[0].Comparison.StrongMatch || len(candidates[0].Comparison.Conflicts) == 0 {
		t.Fatal("conflict explanation", candidates, err)
	}
	if err = s.Ingest(ctx, assetInventory(*bindings[1].LegacyAgentID, "session-1", "restore", time.Now().Add(2*time.Second), d)); err != nil {
		t.Fatal(err)
	}
	fresh, err = s.Asset(ctx, items[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	in.SourceRevision = fresh.Revision
	if _, _, err = s.MergeAssets(ctx, in, "verified-merge"); err != nil {
		t.Fatal(err)
	}
	if err = s.Ingest(ctx, assetInventory(*bindings[1].LegacyAgentID, "session-1", "changed-again", time.Now().Add(3*time.Second), changed)); err != nil {
		t.Fatal(err)
	}
	a, err := s.Asset(ctx, items[0].ID)
	if err != nil || a.ResolutionStatus != "conflict" || a.SourceCount != 2 {
		t.Fatal("new conflict erased or source detached", a, err)
	}
	actor, _ := access.Current(ctx)
	if err = s.AccessLogout(ctx, actor, false, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.SplitAsset(ctx, assets.SplitInput{AssetID: a.ID, Revision: a.Revision, BindingIDs: []string{bindings[1].ID}, CanonicalBindingID: bindings[1].ID, Confirmed: true, Reason: "review"}, "revoked-split"); err != access.ErrForbidden {
		t.Fatal("revoked admin applied split", err)
	}
}

func TestAssetMergeAtomicAuditFailureAndConcurrentStaleRequests(t *testing.T) {
	s, ctx, items, bindings, _ := assetDecisionFixture(t)
	in := assetMergeInput(items, bindings)
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_asset_decision() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture decision failure'; END $$; CREATE TRIGGER reject_asset_decision BEFORE INSERT ON asset_decisions FOR EACH ROW EXECUTE FUNCTION reject_asset_decision()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MergeAssets(ctx, in, "failed-merge"); err == nil {
		t.Fatal("decision failure committed merge")
	}
	for _, original := range items {
		a, err := s.Asset(ctx, original.ID)
		if err != nil || a.SourceCount != 1 || a.Revision != original.Revision || a.LifecycleStatus != "observed" {
			t.Fatal("partial merge survived rollback", a, err)
		}
	}
	if _, err := s.pool.Exec(ctx, `DROP TRIGGER reject_asset_decision ON asset_decisions`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, key := range []string{"concurrent-one", "concurrent-two"} {
		wg.Add(1)
		go func(key string) { defer wg.Done(); _, _, err := s.MergeAssets(ctx, in, key); results <- err }(key)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if err == assets.ErrConflict {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("concurrent stale requests both committed", success, conflict)
	}
}

func TestAssetMergeEntityKindSimulationAndSplitSubsetGuards(t *testing.T) {
	s, ctx, items, bindings, d := assetDecisionFixture(t)
	now := time.Now().Add(time.Second)
	chassis := proto.Clone(d).(*pb.Device)
	chassis.Id = "chassis"
	chassis.Hardware.ResourceType = "Chassis"
	if err := s.Ingest(ctx, assetInventory(*bindings[0].LegacyAgentID, "session-1", "chassis", now, chassis)); err != nil {
		t.Fatal(err)
	}
	group, err := s.Assets(ctx, "chassis", "real", 50, 0)
	if err != nil || len(group) != 1 {
		t.Fatal(group, err)
	}
	in := assetMergeInput(items, bindings)
	in.SourceID = group[0].ID
	in.SourceRevision = group[0].Revision
	if _, _, err = s.MergeAssets(ctx, in, "different-kind"); err != assets.ErrConflict {
		t.Fatal("system/chassis merged", err)
	}
	sim := proto.Clone(d).(*pb.Device)
	sim.Id = "sim-system"
	sim.Source = "simulated"
	if err = s.Ingest(ctx, assetInventory(*bindings[0].LegacyAgentID, "session-1", "simulation", now, sim)); err != nil {
		t.Fatal(err)
	}
	group, err = s.Assets(ctx, "system", "simulated", 50, 0)
	if err != nil || len(group) != 1 {
		t.Fatal(group, err)
	}
	in.SourceID = group[0].ID
	in.SourceRevision = group[0].Revision
	if _, _, err = s.MergeAssets(ctx, in, "different-scope"); err != assets.ErrConflict {
		t.Fatal("simulation merged with real hardware", err)
	}
	if _, _, err = s.SplitAsset(ctx, assets.SplitInput{AssetID: items[0].ID, Revision: items[0].Revision, BindingIDs: []string{bindings[0].ID}, CanonicalBindingID: bindings[0].ID, Confirmed: true, Reason: "split all"}, "split-all"); err != assets.ErrConflict {
		t.Fatal("split emptied source asset", err)
	}
}
