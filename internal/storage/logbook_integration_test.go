package storage

import (
	"fmt"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func logEnvelope(id string, now time.Time) *pb.AgentEnvelope {
	zero := uint32(0)
	no := false
	return &pb.AgentEnvelope{AgentId: "agent-1", SessionId: "session-1", MessageId: id, Sequence: 2, SentAt: timestamppb.New(now),
		Payload: &pb.AgentEnvelope_Logs{Logs: &pb.LogBatch{Source: "linux-file", ObservedAt: timestamppb.New(now), Records: []*pb.LogRecord{{Id: "record-1", DeviceId: "remote-log-host", SourceId: "audit", ObservedAt: timestamppb.New(now), OriginalTimestamp: timestamppb.New(now.Add(-time.Hour)), Severity: &zero, Facility: &zero, Host: "server", Process: "kernel", Message: "Denied execution password=private-log-secret", Raw: "token=private-raw-value", FileIdentity: "inode-1", Audit: &pb.AuditRecord{EventId: "1.1:42", RecordType: "SYSCALL", Uid: &zero, Auid: &zero, Pid: &zero, Success: &no, Executable: "/bin/test", Result: "-13", Host: "server"}}}}}}
}

func TestLogbookPipelineBoundaryAndDatabaseRollback(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	e := logEnvelope("large-batch", now)
	base := e.GetLogs().Records[0]
	e.GetLogs().Records = nil
	for i := 0; i < 260; i++ {
		r := proto.Clone(base).(*pb.LogRecord)
		r.Id = fmt.Sprintf("row-%03d", i)
		r.Audit = nil
		r.Severity = nil
		e.GetLogs().Records = append(e.GetLogs().Records, r)
	}
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	p, err := s.Logs(ctx, logbook.Filter{Limit: 128, SeverityUnknown: true})
	if err != nil || len(p.Items) != 128 || p.NextBeforeID == nil {
		t.Fatalf("pipeline first page: %+v %v", p, err)
	}
	p2, err := s.Logs(ctx, logbook.Filter{Limit: 128, BeforeID: *p.NextBeforeID, SeverityUnknown: true})
	if err != nil || len(p2.Items) != 128 || p2.NextBeforeID == nil || p2.Items[0].ID >= p.Items[127].ID {
		t.Fatal("pipeline second page lost ordering")
	}
	p3, err := s.Logs(ctx, logbook.Filter{Limit: 128, BeforeID: *p2.NextBeforeID, SeverityUnknown: true})
	if err != nil || len(p3.Items) != 4 || p3.NextBeforeID != nil {
		t.Fatal("pipeline tail lost records")
	}
	if _, err = s.pool.Exec(ctx, `ALTER TABLE audit_records ADD CONSTRAINT reject_test_audit CHECK(record_type <> 'FAIL_TEST')`); err != nil {
		t.Fatal(err)
	}
	bad := logEnvelope("database-error", now)
	bad.GetLogs().Records[0].Id = "audit-first"
	r2 := proto.Clone(bad.GetLogs().Records[0]).(*pb.LogRecord)
	r2.Id, r2.Audit.RecordType = "audit-second", "FAIL_TEST"
	bad.GetLogs().Records = append(bad.GetLogs().Records, r2)
	if err = s.Ingest(ctx, bad); err == nil {
		t.Fatal("database error was acknowledged")
	}
	var inbox, batches, audits, records int
	err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_messages WHERE message_id='database-error'),
	(SELECT count(*) FROM log_batches WHERE message_id='database-error'),(SELECT count(*) FROM audit_records),(SELECT count(*) FROM log_records)`).Scan(&inbox, &batches, &audits, &records)
	if err != nil || inbox != 0 || batches != 0 || audits != 0 || records != 260 {
		t.Fatalf("partial database failure committed: %d %d %d %d %v", inbox, batches, audits, records, err)
	}
}

func TestLogbookAuditCursorGroupsAndSourceIsolation(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	e := logEnvelope("interleaved-audit", now)
	base := e.GetLogs().Records[0]
	e.GetLogs().Records = nil
	for i, event := range []string{"A", "B", "B", "A"} {
		r := proto.Clone(base).(*pb.LogRecord)
		r.Id, r.Audit.EventId = fmt.Sprintf("group-row-%d", i), event
		r.Audit.Host = "" // Fall back to the log host consistently in key and API.
		e.GetLogs().Records = append(e.GetLogs().Records, r)
	}
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	first, err := s.AuditEvents(ctx, logbook.Filter{Limit: 1})
	if err != nil || len(first.Items) != 1 || first.Items[0].EventID != "A" || first.Items[0].RecordCount != 2 || first.Items[0].Host != "server" || first.NextBeforeID == nil {
		t.Fatalf("group page1: %+v %v", first, err)
	}
	second, err := s.AuditEvents(ctx, logbook.Filter{Limit: 1, BeforeID: *first.NextBeforeID})
	if err != nil || len(second.Items) != 1 || second.Items[0].EventID != "B" || second.Items[0].RecordCount != 2 || second.NextBeforeID != nil {
		t.Fatalf("group page2 repeats a partial A: %+v %v", second, err)
	}
	other := proto.Clone(e).(*pb.AgentEnvelope)
	other.MessageId, other.GetLogs().Source = "other-source", "syslog"
	if err := s.Ingest(ctx, other); err != nil {
		t.Fatal(err)
	}
	all, err := s.AuditEvents(ctx, logbook.Filter{Limit: 10})
	if err != nil || len(all.Items) != 4 {
		t.Fatal("different sources collided")
	}
}

func TestLogbookSimulatorSessionScopeAndUnknownAuditFields(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	e := logEnvelope("sim-first", now)
	e.GetLogs().Records[0].Source = "simulated"
	e.GetLogs().Records[0].Severity = nil
	e.GetLogs().Records[0].Facility = nil
	e.GetLogs().Records[0].Audit.Success = nil
	e.GetLogs().Records[0].Audit.Uid = nil
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	hello := registration("agent-1", now.Add(time.Second))
	hello.MessageId, hello.SessionId, hello.GetHello().SessionId = "hello-second", "session-2", "session-2"
	if err := s.Ingest(ctx, hello); err != nil {
		t.Fatal(err)
	}
	repeated := proto.Clone(e).(*pb.AgentEnvelope)
	repeated.MessageId, repeated.SessionId = "sim-second", "session-2"
	if err := s.Ingest(ctx, repeated); err != nil {
		t.Fatal(err)
	}
	p, err := s.Logs(ctx, logbook.Filter{Limit: 10, SuccessUnknown: true, SeverityUnknown: true})
	if err != nil || len(p.Items) != 2 || p.Items[0].Audit.UID != nil || p.Items[0].Facility != nil || p.Items[0].Audit.Success != nil {
		t.Fatal("simulator restart or unknown fields lost")
	}
	groups, err := s.AuditEvents(ctx, logbook.Filter{Limit: 10})
	if err != nil || len(groups.Items) != 2 || groups.Items[0].Key == groups.Items[1].Key {
		t.Fatal("two simulator runs merged into one event")
	}
}

func TestLogbookPostgresStorageSearchPrivacyAndOptionalFields(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	e := logEnvelope("logs", now)
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	d, err := s.Device(ctx, "agent-1", "remote-log-host")
	if err != nil || d.Availability.Status != "unknown" || d.Online {
		t.Fatalf("logs invented availability: %+v %v", d, err)
	}
	page, err := s.Logs(ctx, logbook.Filter{AgentID: "agent-1", Q: "denied", Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("full-text search: %+v %v", page, err)
	}
	r := page.Items[0]
	if !r.Redacted || r.Message == "Denied execution password=private-log-secret" || r.Raw == "token=private-raw-value" || r.Severity == nil || *r.Severity != 0 || r.SeverityName != "emergency" || r.Audit.Success == nil || *r.Audit.Success || r.Audit.UID == nil || *r.Audit.UID != 0 || !r.EventAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("projected fields lost: %+v", r)
	}
	p, err := s.Logs(ctx, logbook.Filter{Q: "private-log-secret", Limit: 10})
	if err != nil || len(p.Items) != 0 {
		t.Fatal("secret entered searchable projection")
	}
	summary, err := s.LogsSummary(ctx, logbook.Filter{})
	if err != nil || summary.Total != 1 || summary.AuditFailures != 1 || summary.Severity["emergency"] != 1 || summary.Redacted != 1 {
		t.Fatalf("summary: %+v %v", summary, err)
	}
	zero := uint32(0)
	no := false
	p, err = s.Logs(ctx, logbook.Filter{AuditOnly: true, UID: &zero, Success: &no, Limit: 10})
	if err != nil || len(p.Items) != 1 {
		t.Fatal("false/zero filtering broken")
	}
}

func TestLogbookReplayRereadConflictAndAtomicity(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	original := logEnvelope("original-logs", now)
	if err := s.Ingest(ctx, original); err != nil {
		t.Fatal(err)
	}
	replay := proto.Clone(original).(*pb.AgentEnvelope)
	replay.Sequence = 20
	replay.SentAt = timestamppb.New(now.Add(time.Minute))
	if err := s.Ingest(ctx, replay); err != nil {
		t.Fatal(err)
	}
	replay.MessageId = "reread"
	replay.GetLogs().ObservedAt = timestamppb.New(now.Add(time.Minute))
	replay.GetLogs().Records[0].ObservedAt = timestamppb.New(now.Add(time.Minute))
	if err := s.Ingest(ctx, replay); err != nil {
		t.Fatal(err)
	}
	page, err := s.Logs(ctx, logbook.Filter{Limit: 10})
	if err != nil || len(page.Items) != 1 || !page.Items[0].ObservedAt.Equal(now) {
		t.Fatal("reread duplicated/refreshed record")
	}
	bad := proto.Clone(original).(*pb.AgentEnvelope)
	bad.MessageId = "conflicting-batch"
	fresh := proto.Clone(bad.GetLogs().Records[0]).(*pb.LogRecord)
	fresh.Id = "fresh-before-error"
	bad.GetLogs().Records[0].Message = "different contents"
	bad.GetLogs().Records = append([]*pb.LogRecord{fresh}, bad.GetLogs().Records...)
	if err = s.Ingest(ctx, bad); err != ErrLogConflict {
		t.Fatalf("record conflict accepted: %v", err)
	}
	var inbox, records int
	err = s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_messages WHERE message_id='conflicting-batch'),(SELECT count(*) FROM log_records)`).Scan(&inbox, &records)
	if err != nil || inbox != 0 || records != 1 {
		t.Fatalf("partial log batch committed: %d %d %v", inbox, records, err)
	}
}

func TestLogbookAuditGroupingAndCursorPages(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	e := logEnvelope("audit-group", now)
	r2 := proto.Clone(e.GetLogs().Records[0]).(*pb.LogRecord)
	r2.Id = "path"
	r2.Audit.RecordType = "PATH"
	r2.Audit.Success = nil
	r2.Audit.Uid = nil
	e.GetLogs().Records = append(e.GetLogs().Records, r2)
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	events, err := s.AuditEvents(ctx, logbook.Filter{Limit: 10})
	if err != nil || len(events.Items) != 1 || events.Items[0].RecordCount != 2 || events.Items[0].Outcome != "failure" || events.Items[0].Completeness != "unknown" {
		t.Fatalf("grouping: %+v %v", events, err)
	}
	group := events.Items[0]
	p, err := s.Logs(ctx, logbook.Filter{EventKey: group.Key, Limit: 1})
	if err != nil || len(p.Items) != 1 || p.NextBeforeID == nil {
		t.Fatal("first cursor page")
	}
	first := p.Items[0].ID
	p, err = s.Logs(ctx, logbook.Filter{EventKey: group.Key, BeforeID: *p.NextBeforeID, Limit: 1})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID == first || p.NextBeforeID != nil {
		t.Fatal("last cursor page repeated or lost data")
	}
	zero := uint32(0)
	filtered, err := s.AuditEvents(ctx, logbook.Filter{UID: &zero, Limit: 10})
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].RecordCount != 1 {
		t.Fatal("filtered audit group included records with an unknown uid")
	}
}

func TestLogbookBackfillIdempotentWithoutSessionReset(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	e := logEnvelope("old-archive", now)
	raw, err := wireJSON(e)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO agent_messages(agent_id,message_id,session_id,sequence,sent_at,kind,payload)
	VALUES($1,$2,$3,$4,$5,'logs',$6)`, e.AgentId, e.MessageId, e.SessionId, e.Sequence, e.SentAt.AsTime(), raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.BackfillLogs(ctx, 10)
	if err != nil || out.Processed != 1 || out.Inserted != 1 || out.Failed != 0 {
		t.Fatalf("backfill: %+v %v", out, err)
	}
	again, err := s.BackfillLogs(ctx, 10)
	if err != nil || again.Processed != 0 {
		t.Fatal("backfill duplicated projections")
	}
	agents, err := s.Agents(ctx)
	if err != nil || !agents[0].Connected {
		t.Fatal("backfill reset live session")
	}
}

func TestLogbookBackfillReportsMalformedArchiveWithoutPartialWrites(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	e := logEnvelope("good-archived", now)
	raw, err := wireJSON(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO agent_messages(agent_id,message_id,session_id,sequence,sent_at,kind,payload)
	VALUES('agent-1','bad-archived','session-1',2,$1,'logs','{}'),('agent-1','good-archived','session-1',3,$1,'logs',$2)`, now, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BackfillLogs(ctx, 0); err == nil {
		t.Fatal("unbounded backfill limit accepted")
	}
	out, err := s.BackfillLogs(ctx, 10)
	if err != nil || out.Failed != 1 || out.Processed != 1 || out.Inserted != 1 {
		t.Fatalf("failed backfill not reported: %+v %v", out, err)
	}
	var batches int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM log_batches`).Scan(&batches); err != nil || batches != 1 {
		t.Fatal("malformed archive marked as projected")
	}
}

func TestLogbookAgentIsolationAndLiteralSearch(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, agent := range []string{"agent-1", "agent-2"} {
		if err := s.Ingest(ctx, registration(agent, now)); err != nil {
			t.Fatal(err)
		}
	}
	a := logEnvelope("a", now)
	b := proto.Clone(a).(*pb.AgentEnvelope)
	b.AgentId = "agent-2"
	b.MessageId = "b"
	if err := s.Ingest(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, b); err != nil {
		t.Fatal(err)
	}
	p, err := s.Logs(ctx, logbook.Filter{AgentID: "agent-1", Limit: 10})
	if err != nil || len(p.Items) != 1 {
		t.Fatal("agents overwrote/leaked records")
	}
	p, err = s.Logs(ctx, logbook.Filter{Q: "' ; DROP TABLE devices; --", Limit: 10})
	if err != nil {
		t.Fatal("search syntax executed SQL or errored")
	}
	from, to := now.Add(-2*time.Hour), now
	p, err = s.Logs(ctx, logbook.Filter{Contains: "den", From: &from, To: &to, Limit: 10})
	if err != nil || len(p.Items) != 2 {
		t.Fatalf("bounded literal search failed: %+v %v", p, err)
	}
}
