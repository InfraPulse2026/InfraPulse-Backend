package logbook

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func logFixture(now time.Time) *pb.AgentEnvelope {
	zero := uint32(0)
	no := false
	return &pb.AgentEnvelope{AgentId: "agent", SessionId: "session", MessageId: "message", SentAt: timestamppb.New(now),
		Payload: &pb.AgentEnvelope_Logs{Logs: &pb.LogBatch{ObservedAt: timestamppb.New(now), Source: "linux-file", Records: []*pb.LogRecord{{Id: "stable-file-record", SourceId: "audit", ObservedAt: timestamppb.New(now), OriginalTimestamp: timestamppb.New(now.Add(-time.Hour)), Severity: &zero, Facility: &zero, Host: "host", Message: "password=private-secret", Raw: "Authorization: Bearer super-private-value",
			Audit: &pb.AuditRecord{EventId: "1.1:42", RecordType: "SYSCALL", Uid: &zero, Auid: &zero, Pid: &zero, Success: &no, Executable: "/bin/test", Result: "token=private-result"}}}}}}
}

func TestPreparePrivacyPresenceAndImmutablePayload(t *testing.T) {
	now := time.Now().UTC()
	e := logFixture(now)
	original := proto.Clone(e)
	items, err := Prepare(e, "local:agent", now)
	if err != nil || len(items) != 1 {
		t.Fatalf("prepare: %v %v", items, err)
	}
	r := items[0].Record
	if !r.Redacted || r.DeviceID != "local:agent" || r.TimeBasis != "original_timestamp" || r.Severity == nil || *r.Severity != 0 || r.SeverityName != "emergency" || r.Audit.Success == nil || *r.Audit.Success || r.Audit.UID == nil || *r.Audit.UID != 0 {
		t.Fatalf("presence/redaction lost: %+v", r)
	}
	encoded, _ := json.Marshal(r)
	for _, secret := range []string{"private-secret", "super-private-value", "private-result"} {
		if strings.Contains(string(encoded), secret) || strings.Contains(items[0].SearchText, secret) {
			t.Fatalf("projection leaked %s", secret)
		}
	}
	if !proto.Equal(original, e) {
		t.Fatal("sanitizer changed business payload used by durable dedup")
	}
	retry := proto.Clone(e).(*pb.AgentEnvelope)
	retry.GetLogs().Records[0].ObservedAt = timestamppb.New(now.Add(time.Minute))
	repeated, _ := Prepare(retry, "local:agent", now.Add(time.Minute))
	if !bytes.Equal(repeated[0].Fingerprint, items[0].Fingerprint) {
		t.Fatal("collection timestamp broke stable file cursor dedup")
	}
	retry.GetLogs().Records[0].Message = "password=different-secret"
	changed, _ := Prepare(retry, "local:agent", now)
	if bytes.Equal(changed[0].Fingerprint, items[0].Fingerprint) {
		t.Fatal("redaction masked a business-content conflict")
	}
}

func TestAuditExactGroupingAndUnknownFields(t *testing.T) {
	now := time.Now().UTC()
	e := logFixture(now)
	first := e.GetLogs().Records[0]
	second := proto.Clone(first).(*pb.LogRecord)
	second.Id = "path"
	second.Audit.RecordType = "PATH"
	second.Audit.Success = nil
	second.Audit.Uid = nil
	e.GetLogs().Records = append(e.GetLogs().Records, second)
	items, _ := Prepare(e, "local:agent", now)
	if items[0].Record.Audit.EventKey != items[1].Record.Audit.EventKey {
		t.Fatal("records of exact same event separated")
	}
	second.Audit.Host = "other"
	other, _ := Prepare(e, "local:agent", now)
	if other[0].Record.Audit.EventKey == other[1].Record.Audit.EventKey {
		t.Fatal("different hosts merged")
	}
	if other[1].Record.Audit.UID != nil || other[1].Record.Audit.Success != nil {
		t.Fatal("unknown optional fields replaced with zero/false")
	}
	first.Audit.EventId = ""
	second.Audit.EventId = ""
	second.Audit.Host = ""
	unidentified, _ := Prepare(e, "local:agent", now)
	if unidentified[0].Record.Audit.EventKey == unidentified[1].Record.Audit.EventKey {
		t.Fatal("missing event IDs merged unrelated records")
	}
}

func TestValidationBoundsAndSeverityUnknown(t *testing.T) {
	u := uint32(99)
	if SeverityName(nil) != "unknown" || SeverityName(&u) != "unknown" {
		t.Fatal("unknown severity invented")
	}
	now := time.Now()
	batch := logFixture(now).GetLogs()
	batch.Records[0].Id = strings.Repeat("x", 257)
	if Validate(batch) == nil {
		t.Fatal("unbounded identity accepted")
	}
	batch.Records[0].Id = "id"
	batch.Records = append(batch.Records, nil)
	if Validate(batch) == nil {
		t.Fatal("nil record accepted")
	}
}

func TestTypedSimulationScopeAndAuditGrouping(t *testing.T) {
	now := time.Now().UTC()
	for _, batch := range []bool{false, true} {
		e := logFixture(now)
		meta := &pb.ObservationMetadata{Source: &pb.SourceRef{Type: "file", Provenance: "simulated"}}
		if batch {
			e.GetLogs().Observation = meta
		} else {
			e.GetLogs().Records[0].Observation = meta
		}
		first, err := Prepare(e, "local:agent", now)
		if err != nil || first[0].Scope != "session" {
			t.Fatal("typed simulation treated as real", first, err)
		}
		e.SessionId = "another-run"
		second, err := Prepare(e, "local:agent", now)
		if err != nil || second[0].Scope != "another-run" || first[0].Record.Audit.EventKey == second[0].Record.Audit.EventKey {
			t.Fatal("audit events crossed simulation runs", second, err)
		}
	}
}
