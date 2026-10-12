package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/reports"
)

func reportIncident(t *testing.T, s *Store, id string, from time.Time, open int, resolve *int, source, scope string, occurrence int, ack *int) {
	t.Helper()
	var resolved, ackAt *time.Time
	if resolve != nil {
		v := from.Add(time.Duration(*resolve) * time.Second)
		resolved = &v
	}
	if ack != nil {
		v := from.Add(time.Duration(*ack) * time.Second)
		ackAt = &v
	}
	state := "open"
	if resolved != nil {
		state = "resolved"
	}
	_, err := s.pool.Exec(t.Context(), `INSERT INTO incidents(id,fingerprint,agent_id,device_id,source,scope,rule_id,occurrence,state,severity,opened_at,last_observed_at,updated_at,resolved_at,acknowledged_at,signal) VALUES($1,$2,'agent-1','shared-router',$3,$4,'cpu.sustained',$5,$6,'warning',$7,$7,$7,$8,$9,'{}')`, id, "fixture:"+source+":"+scope, source, scope, occurrence, state, from.Add(time.Duration(open)*time.Second), resolved, ackAt)
	if err != nil {
		t.Fatal(err)
	}
}
func TestReportPeriodClippingRecurrenceAndNotAvailability(t *testing.T) {
	s, ctx, _ := healthFixture(t)
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	to := from.Add(100 * time.Second)
	a, b, boundary, outside, ack := 20, 70, 100, 0, 50
	reportIncident(t, s, "carry", from, -20, &a, "linux", "", 1, nil)
	reportIncident(t, s, "second", from, 30, &b, "linux", "", 2, &ack)
	reportIncident(t, s, "simulation", from, 80, nil, "simulated", "demo-session", 1, nil)
	reportIncident(t, s, "boundary", from, 40, &boundary, "linux", "", 3, nil)
	reportIncident(t, s, "outside-open", from, 100, nil, "remote", "", 1, nil)
	reportIncident(t, s, "outside-resolved", from, -10, &outside, "other", "", 1, nil)
	result, err := s.ReportSummary(ctx, reports.Filter{From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	v := result.Stats
	if v.Overlapping != 4 || v.Opened != 3 || v.CarriedIn != 1 || v.Resolved != 2 || v.ActiveAtEnd != 2 || v.Acknowledged != 1 || math.Abs(v.SummedSeconds-140) > 0.001 {
		t.Fatalf("wrong clipped stats: %+v", v)
	}
	if v.MeanAckSeconds == nil || *v.MeanAckSeconds != 20 || v.MeanResolutionSeconds == nil || *v.MeanResolutionSeconds != 40 {
		t.Fatal("event delays wrong")
	}
	if v.RealEpisodes != 3 || v.SimulatedEpisodes != 1 || len(result.Recurring) != 1 || result.Recurring[0].OpenedOccurrences != 2 {
		t.Fatal("sources/recurrence pooled incorrectly")
	}
	if result.Quality.AvailabilityPercent != nil || result.Quality.CoverageStatus != "received_samples_only_not_uptime" {
		t.Fatal("incident sum masquerades as availability")
	}
	if dir := os.Getenv("REPORT_PROOF_DIR"); dir != "" {
		proof := struct {
			Scenario string          `json:"scenario"`
			Report   reports.Summary `json:"report"`
		}{"synthetic_report_lifecycle_fixture", result}
		encoded, err := json.MarshalIndent(proof, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "report.fixture.json"), append(encoded, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	filtered, err := s.ReportSummary(ctx, reports.Filter{From: from, To: to, Source: "simulated"})
	if err != nil || filtered.Stats.Overlapping != 1 || filtered.Stats.MeanResolutionSeconds != nil {
		t.Fatal("filter/null semantics wrong")
	}
}
func TestReportSnapshotConcurrentIdempotencyAndImmutableContents(t *testing.T) {
	s, ctx, _ := healthFixture(t)
	from := time.Now().Add(-time.Hour).Truncate(time.Second)
	to := from.Add(10 * time.Minute)
	f := reports.Filter{From: from, To: to}
	result := make(chan reports.Snapshot, 4)
	failures := make(chan error, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snapshot, _, err := s.CreateReportSnapshot(ctx, f, "same-request")
			if err != nil {
				failures <- err
			} else {
				result <- snapshot
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	close(result)
	id := ""
	for snapshot := range result {
		if id != "" && snapshot.ID != id {
			t.Fatal("concurrent snapshot duplicated")
		}
		id = snapshot.ID
		if snapshot.Report == nil || snapshot.Report.Stats.Overlapping != 0 {
			t.Fatal("initial snapshot wrong")
		}
	}
	reportIncident(t, s, "later-import", from, 10, nil, "linux", "", 1, nil)
	old, err := s.ReportSnapshot(ctx, id)
	if err != nil || old.Report.Stats.Overlapping != 0 {
		t.Fatal("saved report mutated")
	}
	live, err := s.ReportSummary(ctx, f)
	if err != nil || live.Stats.Overlapping != 1 {
		t.Fatal("live report did not reflect later data")
	}
	replay, hit, err := s.CreateReportSnapshot(ctx, f, "same-request")
	if err != nil || !hit || replay.ID != id || replay.Report.Stats.Overlapping != 0 {
		t.Fatal("replay recomputed saved snapshot")
	}
	f.TopLimit = 30
	if _, _, err = s.CreateReportSnapshot(ctx, f, "same-request"); !errors.Is(err, reports.ErrConflict) {
		t.Fatal("conflicting report request reused key")
	}
	var count int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM access_audit WHERE action='report_snapshot_created'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate/missing report audit")
	}
	items, err := s.ReportSnapshots(ctx, 20, 0)
	if err != nil || len(items) != 1 || items[0].Report != nil {
		t.Fatal("snapshot list includes full payload or wrong count")
	}
}
func TestReportSnapshotAuditFailureAndRevokedActor(t *testing.T) {
	s, ctx, _ := healthFixture(t)
	f := reports.Filter{From: time.Now().Add(-time.Hour), To: time.Now().Add(-time.Minute)}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE access_audit ADD CONSTRAINT reject_report_fixture CHECK(action<>'report_snapshot_created')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateReportSnapshot(ctx, f, "failure"); err == nil {
		t.Fatal("SQL failure absent")
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM report_snapshots`).Scan(&count); err != nil || count != 0 {
		t.Fatal("report persisted without audit")
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE access_audit DROP CONSTRAINT reject_report_fixture`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateReportSnapshot(ctx, f, "failure"); err != nil {
		t.Fatal(err)
	}
	actor, _ := access.Current(ctx)
	if err := s.AccessLogout(ctx, actor, true, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateReportSnapshot(ctx, f, "after-logout"); !errors.Is(err, access.ErrCredentials) {
		t.Fatal("revoked actor created report")
	}
}
func TestReportRecurrenceDoesNotCombineSimulationSessions(t *testing.T) {
	s, ctx, _ := healthFixture(t)
	from := time.Now().Add(-time.Hour)
	to := from.Add(10 * time.Minute)
	for i, scope := range []string{"session-a", "session-b"} {
		end := 30 + i
		reportIncident(t, s, fmt.Sprint("scope", i), from, 10+i, &end, "simulated", scope, 99, nil)
	}
	result, err := s.ReportSummary(ctx, reports.Filter{From: from, To: to})
	if err != nil || len(result.Recurring) != 0 {
		t.Fatal("global occurrence numbers/simulation scopes misrepresented as recurrence")
	}
}
