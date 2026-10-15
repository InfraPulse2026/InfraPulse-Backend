package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assetlogs"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func assetLogsEnvelope(agent, session, message, device, source string, now time.Time) *pb.AgentEnvelope {
	no := false
	return &pb.AgentEnvelope{AgentId: agent, SessionId: session, MessageId: message, Sequence: 3, SentAt: timestamppb.New(now), Payload: &pb.AgentEnvelope_Logs{Logs: &pb.LogBatch{Source: source, ObservedAt: timestamppb.New(now), Records: []*pb.LogRecord{{Id: "record-1", DeviceId: device, Source: source, SourceId: "audit", Host: "claimed-other-host", Process: "auditd", Message: "Denied test operation password=private-message", Raw: "raw audit token=private-raw", ObservedAt: timestamppb.New(now), FileIdentity: "inode-1", Audit: &pb.AuditRecord{EventId: "1.0:5", RecordType: "SYSCALL", Success: &no}, Syslog: &pb.SyslogMetadata{Version: 1, Format: "rfc5424", ParseStatus: "parsed", PeerIp: "192.0.2.50", Transport: "tls", TlsIdentity: "dns:transport.example", Authenticated: true, Managed: true, RawPayload: []byte{0xff, 0xfe}}, Observation: &pb.ObservationMetadata{Source: &pb.SourceRef{Type: "syslog", CollectorId: agent, TargetId: "claim-not-used-for-resolution", Provenance: "real"}, Time: &pb.ObservationTime{ObservedAt: timestamppb.New(now), CollectedAt: timestamppb.New(now), ClockQuality: "unknown"}}}}}}}
}

func TestAssetLogsExactSourceOwnershipClaimTransportAndReplay(t *testing.T) {
	s, ctx, items, bindings, _ := assetDecisionFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, b := range bindings {
		e := assetLogsEnvelope(*b.LegacyAgentID, "session-1", "logs-owned", "system", "syslog", now)
		if err := s.Ingest(ctx, e); err != nil {
			t.Fatal(err)
		}
		replay := proto.Clone(e).(*pb.AgentEnvelope)
		replay.Sequence = 99
		if err := s.Ingest(ctx, replay); err != nil {
			t.Fatal(err)
		}
		replay.MessageId = "logs-reread"
		replay.GetLogs().Records[0].ObservedAt = timestamppb.New(now.Add(time.Second))
		replay.GetLogs().Records[0].Observation.Time.CollectedAt = replay.GetLogs().Records[0].ObservedAt
		if err := s.Ingest(ctx, replay); err != nil {
			t.Fatal(err)
		}
	}
	f := assetlogs.Filter{Log: logbook.Filter{Limit: 10, Q: "denied"}}
	for i, a := range items {
		p, err := s.AssetLogs(ctx, a.ID, f)
		if err != nil || len(p.Items) != 1 || p.Items[0].AgentID != *bindings[i].LegacyAgentID || p.Items[0].SourceInstanceID != bindings[i].SourceInstanceID || p.Items[0].BindingID != bindings[i].ID || p.Items[0].Association != "source_owned_device_reference" {
			t.Fatal("same device IDs crossed source boundary or replay duplicated", p, err)
		}
		r := p.Items[0]
		b, err := json.Marshal(r)
		if err != nil || strings.Contains(string(b), "private-") || strings.Contains(string(b), `"raw":`) || strings.Contains(string(r.Syslog), "//4=") || !r.RawPayloadOmitted {
			t.Fatal("Asset public projection exposed raw or secret", string(b), err)
		}
		if r.Host != "claimed-other-host" || !strings.Contains(string(r.Syslog), "transport.example") || !strings.Contains(string(r.Syslog), "192.0.2.50") || !strings.Contains(string(r.Syslog), `"authenticated":true`) || !strings.Contains(string(r.Observation), "claim-not-used-for-resolution") {
			t.Fatal("claim and transport attribution conflated", r)
		}
		if _, err = s.AssetLog(ctx, items[1-i].ID, r.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("foreign Asset fetched record", err)
		}
		foreign := f
		foreign.SourceInstanceID = bindings[1-i].SourceInstanceID
		p, err = s.AssetLogs(ctx, a.ID, foreign)
		if err != nil || len(p.Items) != 0 {
			t.Fatal("source filter selected foreign records", p, err)
		}
	}
	unknown := assetLogsEnvelope(*bindings[0].LegacyAgentID, "session-1", "unknown-sender", "unbound-device", "syslog", now)
	unknown.GetLogs().Records[0].Host = items[0].CanonicalName
	unknown.GetLogs().Records[0].Observation.Source.TargetId = "system"
	if err := s.Ingest(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	p, err := s.AssetLogs(ctx, items[0].ID, f)
	if err != nil || len(p.Items) != 1 {
		t.Fatal("hostname/SourceRef claim created sender binding", p, err)
	}
	legacy, err := s.Logs(ctx, logbook.Filter{DeviceID: "unbound-device", Limit: 10})
	if err != nil || len(legacy.Items) != 1 {
		t.Fatal("unresolved evidence disappeared from original API", legacy, err)
	}
	if _, err := s.AssetLogs(ctx, "asset_missing", f); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing Asset indistinguishable from empty existing Asset", err)
	}
}

func TestAssetLogsMergedSplitBindingsAndAuditWholeGroupCursor(t *testing.T) {
	s, ctx, items, bindings, _ := assetDecisionFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, b := range bindings {
		e := assetLogsEnvelope(*b.LegacyAgentID, "session-1", "audit-interleaved", "system", "syslog", now)
		base := e.GetLogs().Records[0]
		e.GetLogs().Records = nil
		for i, event := range []string{"A", "B", "B", "A"} {
			r := proto.Clone(base).(*pb.LogRecord)
			r.Id, r.Audit.EventId = fmt.Sprintf("row-%d", i), event
			if i%2 == 1 {
				r.Audit.RecordType = "PATH"
				r.Audit.Success = nil
				r.Message = "file path evidence"
			}
			e.GetLogs().Records = append(e.GetLogs().Records, r)
		}
		if err := s.Ingest(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.MergeAssets(ctx, assetMergeInput(items, bindings), "merge-log-sources"); err != nil {
		t.Fatal(err)
	}
	f := assetlogs.Filter{Log: logbook.Filter{Limit: 1, RecordType: "SYSCALL"}}
	seen := map[string]bool{}
	for page := 0; page < 4; page++ {
		groups, err := s.AssetAuditEvents(ctx, items[0].ID, f)
		if err != nil || len(groups.Items) != 1 || groups.Items[0].RecordCount != 2 || len(groups.Items[0].RecordTypes) != 2 || groups.Items[0].Outcome != "failure" || groups.Items[0].Completeness != "unknown" || seen[groups.Items[0].Key] {
			t.Fatal("audit filtering returned partial/repeated group", groups, err)
		}
		seen[groups.Items[0].Key] = true
		key := groups.Items[0].Key
		detail, err := s.AssetLogs(ctx, items[0].ID, assetlogs.Filter{Log: logbook.Filter{Limit: 10, EventKey: key, AuditOnly: true}})
		if err != nil || len(detail.Items) != 2 {
			t.Fatal("owned group details", detail, err)
		}
		if page < 3 {
			if groups.NextBeforeID == nil {
				t.Fatal("audit cursor lost next group")
			}
			f.Log.BeforeID = *groups.NextBeforeID
		} else if groups.NextBeforeID != nil {
			t.Fatal("audit cursor fabricated more records")
		}
	}
	all, err := s.AssetLogs(ctx, items[0].ID, assetlogs.Filter{Log: logbook.Filter{Limit: 20}})
	if err != nil || len(all.Items) != 8 {
		t.Fatal("merged Asset missing either Collector", all, err)
	}
	alias, err := s.AssetLogs(ctx, items[1].ID, assetlogs.Filter{Log: logbook.Filter{Limit: 20}})
	if err != nil || len(alias.Items) != 0 {
		t.Fatal("historical Asset ID fabricated on merged alias", alias, err)
	}
	a, err := s.Asset(ctx, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	decision, _, err := s.SplitAsset(ctx, assets.SplitInput{AssetID: a.ID, Revision: a.Revision, BindingIDs: []string{bindings[1].ID}, CanonicalBindingID: bindings[1].ID, Confirmed: true, Reason: "split for verified source ownership"}, "split-log-sources")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, decision.TargetID} {
		p, err := s.AssetLogs(ctx, id, assetlogs.Filter{Log: logbook.Filter{Limit: 20}})
		if err != nil || len(p.Items) != 4 {
			t.Fatal("current binding split lost or duplicated old evidence", p, err)
		}
	}
}

func TestAssetLogsSimulationSessionsNeverAttachToRealOrAnotherRun(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, session := range []string{"session-1", "session-2"} {
		hello := registration("agent-1", now)
		hello.SessionId, hello.MessageId, hello.GetHello().SessionId = session, "hello-"+session, session
		if err := s.Ingest(ctx, hello); err != nil {
			t.Fatal(err)
		}
		e := assetLogsEnvelope("agent-1", session, "sim-logs-"+session, "shared-router", "simulated", now)
		e.GetLogs().Records[0].Observation.Source.Provenance = "simulated"
		if err := s.Ingest(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	real := &pb.Device{Id: "shared-router", Name: "real same ID", Source: "snmp"}
	if err := s.Ingest(ctx, assetInventory("agent-1", "session-2", "real-inventory", now, real)); err != nil {
		t.Fatal(err)
	}
	sim, err := s.Assets(ctx, "", "simulated", 50, 0)
	if err != nil || len(sim) != 2 {
		t.Fatal("simulation fixture", sim, err)
	}
	for _, a := range sim {
		b, err := s.AssetBindings(ctx, a.ID, false, 50, 0)
		if err != nil || len(b) != 1 {
			t.Fatal(b, err)
		}
		p, err := s.AssetLogs(ctx, a.ID, assetlogs.Filter{Log: logbook.Filter{Limit: 10}})
		if err != nil || len(p.Items) != 1 || p.Items[0].RecordScope != b[0].Scope {
			t.Fatal("simulation session collision", p, err)
		}
		other := "session-1"
		if b[0].Scope == other {
			other = "session-2"
		}
		p, err = s.AssetLogs(ctx, a.ID, assetlogs.Filter{Log: logbook.Filter{Limit: 10}, RecordScope: &other})
		if err != nil || len(p.Items) != 0 {
			t.Fatal("scope filter crossed run", p, err)
		}
	}
	reals, err := s.Assets(ctx, "", "real", 50, 0)
	if err != nil || len(reals) != 1 {
		t.Fatal("real fixture", reals, err)
	}
	p, err := s.AssetLogs(ctx, reals[0].ID, assetlogs.Filter{Log: logbook.Filter{Limit: 10}})
	if err != nil || len(p.Items) != 0 {
		t.Fatal("simulated log attached to real binding", p, err)
	}
}

func TestAssetLogRawLiveAdminSessionAuditRollbackAndOwnedRecord(t *testing.T) {
	s, ctx, items, bindings, _ := assetDecisionFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, assetLogsEnvelope(*bindings[0].LegacyAgentID, "session-1", "raw-record", "system", "syslog", now)); err != nil {
		t.Fatal(err)
	}
	p, err := s.AssetLogs(ctx, items[0].ID, assetlogs.Filter{Log: logbook.Filter{Limit: 10}})
	if err != nil || len(p.Items) != 1 {
		t.Fatal(p, err)
	}
	id := p.Items[0].ID
	admin, _ := access.Current(ctx)
	for _, role := range []string{"viewer", "operator"} {
		fake := admin
		fake.User.Role = role
		if _, err := s.AssetLogRaw(access.WithPrincipal(t.Context(), fake), items[0].ID, id); !errors.Is(err, access.ErrForbidden) {
			t.Fatal("non-admin raw access", role, err)
		}
	}
	if _, err := s.AssetLogRaw(t.Context(), items[0].ID, id); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("missing HTTP identity bypassed raw role", err)
	}
	if _, err := s.AssetLogRaw(ctx, items[1].ID, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign Asset raw record", err)
	}
	meta := access.Meta{RequestID: "raw-read", Peer: "test"}
	r, err := s.AssetLogRaw(access.WithMeta(ctx, meta), items[0].ID, id)
	if err != nil || strings.Contains(r.Raw, "private-") || !strings.Contains(r.Raw, "[REDACTED]") || !r.Redacted || !r.RawPayloadAvailable || !r.ArchivePayloadNotExposed {
		t.Fatal("privileged stored raw redaction/metadata", r, err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM access_audit WHERE action='asset_log_raw_read' AND request_id='raw-read'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("raw access was not audited", count, err)
	}
	if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_raw_read_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='asset_log_raw_read' THEN RAISE EXCEPTION 'fixture audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_raw_read_audit BEFORE INSERT ON access_audit FOR EACH ROW EXECUTE FUNCTION reject_raw_read_audit()`); err != nil {
		t.Fatal(err)
	}
	r, err = s.AssetLogRaw(ctx, items[0].ID, id)
	if err == nil || r.Raw != "" {
		t.Fatal("audit failure returned privileged payload", r, err)
	}
	if _, err := s.pool.Exec(ctx, `DROP TRIGGER reject_raw_read_audit ON access_audit`); err != nil {
		t.Fatal(err)
	}
	if err := s.AccessLogout(ctx, admin, false, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AssetLogRaw(ctx, items[0].ID, id); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("revoked admin session still read raw", err)
	}
}

func TestLogRawUnresolvedRecordLiveDatabaseRoleAndSourceIsolation(t *testing.T) {
	s := integrationStore(t)
	svc, admin := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), admin)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.Ingest(ctx, registration("agent-1", now)); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, assetLogsEnvelope("agent-1", "session-1", "unresolved-raw", "unbound-device", "syslog", now)); err != nil {
		t.Fatal(err)
	}
	p, err := s.Logs(ctx, logbook.Filter{DeviceID: "unbound-device", Limit: 10})
	if err != nil || len(p.Items) != 1 {
		t.Fatal(p, err)
	}
	id := p.Items[0].ID
	r, err := s.LogRaw(ctx, id, "agent-1")
	if err != nil || r.AssetID != "" || r.BindingID != "" || r.SourceInstanceID == "" || strings.Contains(r.Raw, "private-") || !strings.Contains(r.Raw, "[REDACTED]") || !r.ArchivePayloadNotExposed {
		t.Fatal("unresolved raw guessed binding or lost redaction", r, err)
	}
	if _, err := s.LogRaw(ctx, id, "another-collector"); !errors.Is(err, ErrNotFound) {
		t.Fatal("raw agent restriction bypassed", err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM access_audit WHERE action='log_raw_read'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("generic raw access not audited", count, err)
	}
	login, err := s.AccessLoginRecord(ctx, admin.User.Username)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AccessCreateUser(ctx, admin, "readonly", "viewer", login.PasswordHash, false, access.Meta{}); err != nil {
		t.Fatal(err)
	}
	viewer, err := svc.Login(ctx, "readonly", fixturePassword, access.Meta{Peer: "test-viewer"})
	if err != nil {
		t.Fatal(err)
	}
	// Neither a forged cached role nor an admin identity carrying somebody
	// else's session hash may substitute for the live database authority.
	viewer.User.Role = "admin"
	if _, err := s.LogRaw(access.WithPrincipal(t.Context(), viewer), id, ""); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("claimed role bypassed live database role", err)
	}
	fake := admin
	fake.SessionHash = viewer.SessionHash
	if _, err := s.LogRaw(access.WithPrincipal(t.Context(), fake), id, ""); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("foreign session accepted for admin", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE access_sessions SET expires_at=now()-interval '1 second' WHERE token_hash=$1`, admin.SessionHash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LogRaw(ctx, id, ""); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("expired admin session read raw", err)
	}
}

func TestAssetLogsRetainedTypedSimulationUsesOriginalSession(t *testing.T) {
	s, ctx, items, bindings, _ := assetDecisionFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	e := assetLogsEnvelope(*bindings[0].LegacyAgentID, "session-1", "retained-typed-sim", "system", "syslog", now)
	e.GetLogs().Records[0].Observation.Source.Provenance = "simulated"
	if err := s.Ingest(ctx, e); err != nil {
		t.Fatal(err)
	}
	// Emulate the pre-B8 projection without changing its immutable envelope/key.
	if _, err := s.pool.Exec(ctx, `UPDATE log_records SET record_scope='' WHERE message_id=$1`, e.MessageId); err != nil {
		t.Fatal(err)
	}
	reread := proto.Clone(e).(*pb.AgentEnvelope)
	reread.MessageId = "retained-sim-reread"
	if err := s.Ingest(ctx, reread); err != nil {
		t.Fatal("reread of historical typed simulation", err)
	}
	page, err := s.AssetLogs(ctx, items[0].ID, assetlogs.Filter{Log: logbook.Filter{Limit: 10}})
	if err != nil || len(page.Items) != 0 {
		t.Fatal("retained typed simulation attached to real inventory", page, err)
	}
	legacy, err := s.Logs(ctx, logbook.Filter{Limit: 10})
	if err != nil || len(legacy.Items) != 1 {
		t.Fatal("historical evidence disappeared", legacy, err)
	}
	raw, err := s.LogRaw(ctx, legacy.Items[0].ID, "")
	if err != nil || raw.RecordScope != "session-1" || raw.AssetID != "" {
		t.Fatal("raw reader guessed real Asset for simulation", raw, err)
	}
	d := &pb.Device{Id: "system", Name: "simulated same ID", Source: "simulated"}
	if err = s.Ingest(ctx, assetInventory(e.AgentId, e.SessionId, "typed-sim-inventory", now, d)); err != nil {
		t.Fatal(err)
	}
	var simAsset string
	if err = s.pool.QueryRow(ctx, `SELECT asset_id FROM asset_source_bindings WHERE legacy_agent_id=$1 AND external_id='system' AND scope='session-1'`, e.AgentId).Scan(&simAsset); err != nil {
		t.Fatal(err)
	}
	page, err = s.AssetLogs(ctx, simAsset, assetlogs.Filter{Log: logbook.Filter{Limit: 10}})
	if err != nil || len(page.Items) != 1 || page.Items[0].RecordScope != "session-1" || page.Items[0].Audit.EventKey != legacy.Items[0].Audit.EventKey {
		t.Fatal("historical group/session changed", page, err)
	}
}
