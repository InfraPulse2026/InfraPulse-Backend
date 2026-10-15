package storage

import (
	"encoding/json"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/reports"
	"testing"
	"time"
)

func TestReportV2ChronicDistinctCasesScopeSnapshotAndSchedule(t *testing.T) {
	s, ctx, _, bindings, _ := assetDecisionFixture(t)
	agent := *bindings[0].LegacyAgentID
	asset := bindings[0].AssetID
	now := time.Now().UTC().Add(-time.Second)
	emit := func(recovery bool) {
		t.Helper()
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		a := health.Assessment{RuleID: "cpu.sustained", RuleVersion: "1", Status: "warning", Severity: "warning", ObservedAt: &now, Evidence: []health.Point{}}
		if recovery {
			a.Status = "ok"
			a.RecoveryEligible = true
		}
		raw, _ := json.Marshal(a)
		if err = applyIncident(ctx, tx, agent, "system", "linux", "", a, raw); err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err = s.ProcessCorrelation(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		emit(false)
		emit(true)
	}
	f := reports.V2Filter{From: now.Add(-time.Hour), To: time.Now().UTC(), AssetID: asset, Scope: "real"}
	summary, err := s.ReportV2Summary(ctx, f)
	if err != nil || summary.Stats.Cases != 3 || summary.Stats.Signals != 3 || summary.Stats.Assets != 1 || len(summary.Chronic) != 1 || summary.Chronic[0].Cases != 3 {
		t.Fatal(summary, err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO physical_locations(id,kind,name) VALUES('site_main','site','Main'); INSERT INTO physical_locations(id,kind,parent_id,name) VALUES('building_main','building','site_main','Building'); INSERT INTO physical_locations(id,kind,parent_id,name) VALUES('room_main','room','building_main','Room')`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO asset_placements(asset_id,mode,location_id) VALUES($1,'outside_rack','room_main')`, asset); err != nil {
		t.Fatal(err)
	}
	f.SiteID = "site_main"
	summary, err = s.ReportV2Summary(ctx, f)
	if err != nil || summary.Stats.Cases != 3 {
		t.Fatal("site scope", summary, err)
	}
	snapshot, replay, err := s.CreateReportV2Snapshot(ctx, f, "unified-snapshot")
	if err != nil || replay {
		t.Fatal(snapshot, err)
	}
	again, replay, err := s.CreateReportV2Snapshot(ctx, f, "unified-snapshot")
	if err != nil || !replay || again.ID != snapshot.ID {
		t.Fatal("snapshot replay", again, err)
	}
	if _, err = s.ReportSnapshot(ctx, snapshot.ID); !errors.Is(err, reports.ErrNotFound) {
		t.Fatal("v2 exposed as legacy shape", err)
	}
	schedule, err := s.SaveReportSchedule(ctx, "", reports.ScheduleInput{Name: "Daily main", Enabled: true, Cadence: "daily", AssetID: asset, Scope: "real", Confirmed: true, Reason: "approved recurring site report"})
	if err != nil {
		t.Fatal(err)
	}
	count, err := s.ProcessScheduledReports(ctx, time.Now().UTC())
	if err != nil || count != 1 {
		t.Fatal("scheduled run", count, err)
	}
	count, err = s.ProcessScheduledReports(ctx, time.Now().UTC())
	if err != nil || count != 0 {
		t.Fatal("duplicate scheduled period", count, err)
	}
	schedules, err := s.ReportSchedules(ctx, 100, 0)
	if err != nil || len(schedules) != 1 || schedules[0].LastSnapshotID == nil {
		t.Fatal(schedules, err)
	}
	actor, _ := access.Current(ctx)
	if _, err = s.pool.Exec(ctx, `UPDATE access_users SET role='viewer' WHERE id=$1`, actor.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE report_schedules SET next_run_at=now()-interval '1 second' WHERE id=$1`, schedule.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProcessScheduledReports(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	schedules, err = s.ReportSchedules(ctx, 100, 0)
	if err != nil || schedules[0].Failure != "owner_access_revoked" || schedules[0].Definition.Enabled {
		t.Fatal("revoked schedule continued", schedules, err)
	}
	if _, _, err = s.CreateReportV2Snapshot(ctx, f, "revoked-snapshot"); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("revoked snapshot accepted", err)
	}
}
