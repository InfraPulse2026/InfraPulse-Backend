package storage

import (
	"fmt"
	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/telephony"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"
)

func sipEnvelope(id string, now time.Time) *pb.AgentEnvelope {
	code := uint32(401)
	zero := 0.0
	return &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: id, Sequence: 2, SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_Telephony{Telephony: &pb.TelephonyUpdate{Source: "sip.options", ObservedAt: timestamppb.New(now), Targets: []*pb.TelephonyHealth{{TargetId: "pbx-main", Address: "pbx.example.net:5061", Transport: "tls", Reachable: true, ResponseCode: &code, LatencyMs: &zero, ObservedAt: timestamppb.New(now), State: "degraded", ErrorCategory: "authentication_required", Source: "sip.options"}}}}}
}

func TestSIPPostgresProjectionAndNullZeroSemantics(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, sipEnvelope("first-sip", now)); err != nil {
		t.Fatal(err)
	}
	card, err := s.TelephonyTarget(ctx, "agent-1", "pbx-main")
	if err != nil || card.Health.Status != "degraded" || card.Latest == nil || card.Latest.ResponseCode == nil || *card.Latest.ResponseCode != 401 || card.Latest.LatencyMS == nil || *card.Latest.LatencyMS != 0 || card.Transport != "tls" || card.Monitoring.ExpectedIntervalSeconds != 30 {
		t.Fatalf("SIP fields lost %+v %v", card, err)
	}
	failed := sipEnvelope("failed-sip", now.Add(time.Second))
	h := failed.GetTelephony().Targets[0]
	h.Reachable, h.State, h.ErrorCategory = false, "down", "timeout"
	h.ResponseCode, h.LatencyMs = nil, nil
	h.ConsecutiveFailures = 3
	if err = s.Ingest(ctx, failed); err != nil {
		t.Fatal(err)
	}
	card, err = s.TelephonyTarget(ctx, "agent-1", "pbx-main")
	if err != nil || card.Health.Status != "down" || card.Latest.ResponseCode != nil || card.Latest.LatencyMS != nil || *card.Health.ReportedReachable {
		t.Fatalf("missing latency invented zero or failure ignored %+v %v", card, err)
	}
	device, err := s.Device(ctx, "agent-1", "shared-router")
	if err != nil || !device.Online {
		t.Fatal("SIP failure changed device availability")
	}
	var devices int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM devices`).Scan(&devices); err != nil || devices != 1 {
		t.Fatal("SIP target was made into a device")
	}
}

func TestSIPReplayConflictAndChronology(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	original := sipEnvelope("sip", now)
	if err := s.Ingest(ctx, original); err != nil {
		t.Fatal(err)
	}
	replay := proto.Clone(original).(*pb.AgentEnvelope)
	replay.Sequence = 9
	replay.SentAt = timestamppb.New(now.Add(time.Minute))
	if err := s.Ingest(ctx, replay); err != nil {
		t.Fatal(err)
	}
	replay.GetTelephony().Targets[0].LatencyMs = nil
	if err := s.Ingest(ctx, replay); err != ErrMessageConflict {
		t.Fatal("same message ID changed SIP payload")
	}
	old := sipEnvelope("late-old", now.Add(-time.Hour))
	old.GetTelephony().Targets[0].State = "up"
	if err := s.Ingest(ctx, old); err != nil {
		t.Fatal(err)
	}
	card, err := s.TelephonyTarget(ctx, "agent-1", "pbx-main")
	if err != nil || card.Latest.MessageID != "sip" {
		t.Fatal("late report replaced latest observation")
	}
	history, err := s.TelephonyHistory(ctx, telephony.HistoryFilter{AgentID: "agent-1", TargetID: "pbx-main", Limit: 10})
	if err != nil || len(history.Items) != 2 || history.Items[0].MessageID != "late-old" {
		t.Fatal("history/replay lost insertion chronology")
	}
}

func TestSIPFutureClockCannotPoisonCurrentOrRecovery(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	future := sipEnvelope("future-only", now.Add(24*time.Hour))
	if err := s.Ingest(ctx, future); err != nil {
		t.Fatal(err)
	}
	card, err := s.TelephonyTarget(ctx, "agent-1", "pbx-main")
	if err != nil || card.Latest != nil || card.ClockIssue == nil || card.Health.Status != "unknown" || card.Health.Reason != "clock_skew" {
		t.Fatalf("future invented qualified health %+v %v", card, err)
	}
	good := sipEnvelope("good-after-clock-reset", now)
	good.GetTelephony().Targets[0].State = "healthy"
	if err := s.Ingest(ctx, good); err != nil {
		t.Fatal(err)
	}
	card, err = s.TelephonyTarget(ctx, "agent-1", "pbx-main")
	if err != nil || card.Health.Status != "up" || card.Health.ClockSkew || card.Latest.MessageID != good.MessageId {
		t.Fatal("normal clock could not recover")
	}
	future.MessageId = "new-clock-error"
	future.GetTelephony().Targets[0].State = "down"
	future.GetTelephony().Targets[0].Reachable = false
	future.GetTelephony().Targets[0].ResponseCode = nil
	if err := s.Ingest(ctx, future); err != nil {
		t.Fatal(err)
	}
	card, err = s.TelephonyTarget(ctx, "agent-1", "pbx-main")
	if err != nil || card.Health.Status != "up" || !card.Health.ClockSkew || card.Latest.MessageID != good.MessageId {
		t.Fatal("future error replaced good report")
	}
	summary, err := s.TelephonySummary(ctx, "agent-1")
	if err != nil || summary.Total != 1 || summary.Up != 1 || summary.ClockSkew != 1 {
		t.Fatalf("summary disagrees with card %+v %v", summary, err)
	}
}

func TestSIPDailyPolicyExpiryAndDisconnect(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	old := sipEnvelope("old-but-daily", now.Add(-10*time.Hour))
	old.GetTelephony().Targets[0].State = "up"
	if err := s.Ingest(ctx, old); err != nil {
		t.Fatal(err)
	}
	card, err := s.TelephonyTarget(ctx, "agent-1", "pbx-main")
	if err != nil || card.Health.Status != "stale" {
		t.Fatal("old report did not expire")
	}
	if err := s.UpdateTelephonyMonitoring(ctx, "agent-1", "pbx-main", 86400); err != nil {
		t.Fatal(err)
	}
	card, err = s.TelephonyTarget(ctx, "agent-1", "pbx-main")
	if err != nil || card.Health.Status != "up" || card.Monitoring.StaleAfterSeconds != 172830 || card.Monitoring.ApplicationStatus != "unverified" {
		t.Fatal("daily expectation or manual setting lost")
	}
	if err := s.UpdateTelephonyMonitoring(ctx, "agent-1", "pbx-main", 2); err == nil {
		t.Fatal("invalid interval persisted")
	}
	if err := s.UpdateTelephonyMonitoring(ctx, "agent-1", "no-such-target", 30); err != ErrNotFound {
		t.Fatal("policy created fake target")
	}
	if err := s.Disconnect(ctx, "agent-1", "session-1"); err != nil {
		t.Fatal(err)
	}
	card, err = s.TelephonyTarget(ctx, "agent-1", "pbx-main")
	if err != nil || card.Health.Status != "stale" || card.Health.Reason != "agent_disconnected" || *card.Health.ReportedState != "up" {
		t.Fatal("disconnect erased SIP report")
	}
}

func TestSIPTargetIsolationAmbiguityAndFilters(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	for _, agent := range []string{"agent-1", "agent-2"} {
		if err := s.Ingest(ctx, registration(agent, now)); err != nil {
			t.Fatal(err)
		}
		e := sipEnvelope("same-message", now)
		e.AgentId = agent
		if agent == "agent-2" {
			e.GetTelephony().Targets[0].Transport = "udp"
		}
		if err := s.Ingest(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.TelephonyTarget(ctx, "", "pbx-main"); err != ErrAmbiguous {
		t.Fatal("ambiguous target resolved arbitrarily")
	}
	if _, err := s.TelephonyTarget(ctx, "agent-1", "missing"); err != ErrNotFound {
		t.Fatal("missing target lacks 404")
	}
	items, err := s.TelephonyTargets(ctx, telephony.Filter{Source: "sip.options", Transport: "tls", Limit: 10})
	if err != nil || len(items) != 1 || items[0].AgentID != "agent-1" {
		t.Fatal("source/transport filtering broken")
	}
	summary, err := s.TelephonySummary(ctx, "")
	if err != nil || summary.Total != 2 || summary.Degraded != 2 {
		t.Fatal("Agent identities merged in summary")
	}
}

func TestSIPHistoryCursorTimeRangeAndEmptyBatch(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := s.Ingest(ctx, sipEnvelope(fmt.Sprintf("point-%d", i), now.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	from, to := now.Add(time.Second), now.Add(4*time.Second)
	page, err := s.TelephonyHistory(ctx, telephony.HistoryFilter{AgentID: "agent-1", TargetID: "pbx-main", From: &from, To: &to, Limit: 2})
	if err != nil || len(page.Items) != 2 || page.NextBeforeID == nil || page.Items[0].MessageID != "point-3" {
		t.Fatalf("first history page %+v %v", page, err)
	}
	tail, err := s.TelephonyHistory(ctx, telephony.HistoryFilter{AgentID: "agent-1", TargetID: "pbx-main", From: &from, To: &to, BeforeID: *page.NextBeforeID, Limit: 2})
	if err != nil || len(tail.Items) != 1 || tail.Items[0].MessageID != "point-1" || tail.NextBeforeID != nil {
		t.Fatal("history boundary or cursor lost rows")
	}
	empty := sipEnvelope("empty-update", now)
	empty.GetTelephony().Targets = nil
	empty.GetTelephony().Source = ""
	if err := s.Ingest(ctx, empty); err != nil {
		t.Fatal(err)
	}
	card, err := s.TelephonyTarget(ctx, "agent-1", "pbx-main")
	if err != nil || card.Latest.MessageID != "point-4" {
		t.Fatal("empty batch removed current target")
	}
}

func TestSIPAtomicSQLFailureAndInvalidBatch(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `ALTER TABLE telephony_samples ADD CONSTRAINT reject_test_target CHECK(target_id <> 'fail-test')`); err != nil {
		t.Fatal(err)
	}
	bad := sipEnvelope("bad-second-row", now)
	second := proto.Clone(bad.GetTelephony().Targets[0]).(*pb.TelephonyHealth)
	second.TargetId = "fail-test"
	bad.GetTelephony().Targets = append(bad.GetTelephony().Targets, second)
	if err := s.Ingest(ctx, bad); err == nil {
		t.Fatal("SQL failure accepted")
	}
	var inbox, batches, targets, samples int
	err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_messages WHERE message_id='bad-second-row'),(SELECT count(*) FROM telephony_batches),(SELECT count(*) FROM telephony_targets),(SELECT count(*) FROM telephony_samples)`).Scan(&inbox, &batches, &targets, &samples)
	if err != nil || inbox != 0 || batches != 0 || targets != 0 || samples != 0 {
		t.Fatalf("partial SIP batch committed %d/%d/%d/%d %v", inbox, batches, targets, samples, err)
	}
	bad.MessageId = "invalid-duplicate"
	bad.GetTelephony().Targets[1].TargetId = "pbx-main"
	if err := s.Ingest(ctx, bad); err != ErrInvalidTelephony {
		t.Fatal("duplicate target silently overwritten")
	}
}

func TestSIPMaximumBatchAndPolicyPreservationAcrossMigrate(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	e := sipEnvelope("256-targets", now)
	base := e.GetTelephony().Targets[0]
	e.GetTelephony().Targets = nil
	for i := 0; i < 256; i++ {
		h := proto.Clone(base).(*pb.TelephonyHealth)
		h.TargetId = fmt.Sprintf("target-%03d", i)
		e.GetTelephony().Targets = append(e.GetTelephony().Targets, h)
	}
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	items, err := s.TelephonyTargets(ctx, telephony.Filter{AgentID: "agent-1", Limit: 256})
	if err != nil || len(items) != 256 {
		t.Fatalf("large SIP batch %+v %v", len(items), err)
	}
	if err := s.UpdateTelephonyMonitoring(ctx, "agent-1", "target-000", 3); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	card, err := s.TelephonyTarget(ctx, "agent-1", "target-000")
	if err != nil || card.Latest == nil || card.Monitoring.ExpectedIntervalSeconds != 3 {
		t.Fatal("restart migration erased samples/policy")
	}
}
