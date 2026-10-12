package storage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/jackc/pgx/v5"
)

const logColumns = `l.id,l.agent_id,l.device_id,l.record_id,l.message_id,l.source,l.source_id,l.event_at,l.observed_at,l.original_timestamp,
l.received_at,l.time_basis,l.severity,l.facility,l.host,l.process,l.process_id,l.message,l.raw,l.truncated,l.redacted,l.file_identity,l.file_offset,
a.log_id,a.event_key,a.event_id,a.record_type,a.uid,a.auid,a.pid,a.executable,a.syscall,a.success,a.result,a.host,
l.syslog_metadata,l.observation_metadata,l.raw_payload_omitted`
const logJoin = ` FROM log_records l LEFT JOIN audit_records a ON a.log_id=l.id `

func logWhere(f logbook.Filter, cursor bool) (string, []any) {
	clauses := []string{"true"}
	args := []any{}
	add := func(expr string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(expr, len(args)))
	}
	for _, pair := range [][2]string{{"l.agent_id", f.AgentID}, {"l.device_id", f.DeviceID}, {"l.source", f.Source}, {"l.source_id", f.SourceID}, {"l.host", f.Host}, {"l.process", f.Process}, {"a.record_type", f.RecordType}, {"a.event_id", f.EventID}, {"a.event_key", f.EventKey}, {"a.executable", f.Executable}} {
		if pair[1] != "" {
			add(pair[0]+"=$%d", pair[1])
		}
	}
	if f.Q != "" {
		add("l.search_vector @@ websearch_to_tsquery('simple',$%d)", f.Q)
	}
	if f.Contains != "" {
		add("position(lower($%d) in lower(l.search_text))>0", f.Contains)
	}
	if f.From != nil {
		add("l.event_at >= $%d", *f.From)
	}
	if f.To != nil {
		add("l.event_at < $%d", *f.To)
	}
	if f.Severity != nil {
		add("l.severity=$%d", *f.Severity)
	}
	if f.SeverityUnknown {
		clauses = append(clauses, "(l.severity IS NULL OR l.severity>7)")
	}
	if f.Facility != nil {
		add("l.facility=$%d", *f.Facility)
	}
	if f.UID != nil {
		add("a.uid=$%d", *f.UID)
	}
	if f.AUID != nil {
		add("a.auid=$%d", *f.AUID)
	}
	if f.PID != nil {
		add("a.pid=$%d", *f.PID)
	}
	if f.Success != nil {
		add("a.success=$%d", *f.Success)
	}
	if f.SuccessUnknown {
		clauses = append(clauses, "a.log_id IS NOT NULL AND a.success IS NULL")
	}
	if f.AuditOnly {
		clauses = append(clauses, "a.log_id IS NOT NULL")
	}
	if cursor && f.BeforeID > 0 {
		add("l.id < $%d", f.BeforeID)
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func scanLog(row interface{ Scan(...any) error }) (logbook.Record, error) {
	var r logbook.Record
	var auditID *int64
	var eventKey, eventID, recordType, exe, syscall, result, host *string
	var uid, auid, pid *uint32
	var success *bool
	err := row.Scan(&r.ID, &r.AgentID, &r.DeviceID, &r.RecordID, &r.MessageID, &r.Source, &r.SourceID, &r.EventAt, &r.ObservedAt, &r.OriginalTimestamp,
		&r.ReceivedAt, &r.TimeBasis, &r.Severity, &r.Facility, &r.Host, &r.Process, &r.ProcessID, &r.Message, &r.Raw, &r.Truncated, &r.Redacted, &r.FileIdentity, &r.Offset,
		&auditID, &eventKey, &eventID, &recordType, &uid, &auid, &pid, &exe, &syscall, &success, &result, &host, &r.Syslog, &r.Observation, &r.RawPayloadOmitted)
	if err != nil {
		return r, err
	}
	r.EventAt, r.ObservedAt, r.ReceivedAt = r.EventAt.UTC(), r.ObservedAt.UTC(), r.ReceivedAt.UTC()
	if r.OriginalTimestamp != nil {
		v := r.OriginalTimestamp.UTC()
		r.OriginalTimestamp = &v
	}
	r.SeverityName = logbook.SeverityName(r.Severity)
	if auditID != nil {
		r.Audit = &logbook.Audit{EventKey: *eventKey, EventID: *eventID, RecordType: *recordType, UID: uid, AUID: auid, PID: pid, Executable: *exe, Syscall: *syscall, Success: success, Result: *result, Host: *host}
	}
	return r, nil
}

func (s *Store) Logs(ctx context.Context, f logbook.Filter) (logbook.Page, error) {
	where, args := logWhere(f, true)
	args = append(args, f.Limit+1)
	rows, err := s.pool.Query(ctx, "SELECT "+logColumns+logJoin+where+fmt.Sprintf(" ORDER BY l.id DESC LIMIT $%d", len(args)), args...)
	page := logbook.Page{Items: []logbook.Record{}, Limit: f.Limit}
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanLog(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, r)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > f.Limit {
		page.Items = page.Items[:f.Limit]
		id := page.Items[len(page.Items)-1].ID
		page.NextBeforeID = &id
	}
	return page, nil
}

func (s *Store) Log(ctx context.Context, id int64, agentID string) (logbook.Record, error) {
	r, err := scanLog(s.pool.QueryRow(ctx, "SELECT "+logColumns+logJoin+" WHERE l.id=$1 AND ($2='' OR l.agent_id=$2)", id, agentID))
	if err == pgx.ErrNoRows {
		return r, ErrNotFound
	}
	return r, err
}

func (s *Store) LogsSummary(ctx context.Context, f logbook.Filter) (logbook.Summary, error) {
	where, args := logWhere(f, false)
	query := `SELECT count(*),count(a.log_id),count(*) FILTER(WHERE a.success=false),
	count(*) FILTER(WHERE l.redacted),count(*) FILTER(WHERE l.truncated),
	count(*) FILTER(WHERE l.severity=0),count(*) FILTER(WHERE l.severity=1),
	count(*) FILTER(WHERE l.severity=2),count(*) FILTER(WHERE l.severity=3),
	count(*) FILTER(WHERE l.severity=4),count(*) FILTER(WHERE l.severity=5),
	count(*) FILTER(WHERE l.severity=6),count(*) FILTER(WHERE l.severity=7),
	count(*) FILTER(WHERE l.severity IS NULL OR l.severity>7)` + logJoin + where
	summary := logbook.Summary{GeneratedAt: time.Now().UTC(), Severity: map[string]int64{}}
	counts := make([]int64, 9)
	dest := []any{&summary.Total, &summary.AuditRecords, &summary.AuditFailures, &summary.Redacted, &summary.Truncated}
	for i := range counts {
		dest = append(dest, &counts[i])
	}
	if err := s.pool.QueryRow(ctx, query, args...).Scan(dest...); err != nil {
		return summary, err
	}
	for i, count := range counts {
		v := uint32(i)
		summary.Severity[logbook.SeverityName(&v)] = count
	}
	return summary, nil
}

type AuditPage struct {
	Items        []logbook.AuditEvent `json:"items"`
	NextBeforeID *int64               `json:"next_before_id"`
	Limit        int                  `json:"limit"`
}

func (s *Store) AuditEvents(ctx context.Context, f logbook.Filter) (AuditPage, error) {
	f.AuditOnly = true
	where, args := logWhere(f, false)
	having := ""
	if f.BeforeID > 0 {
		args = append(args, f.BeforeID)
		having = fmt.Sprintf(" HAVING max(l.id)<$%d", len(args))
	}
	args = append(args, f.Limit+1)
	query := `SELECT max(l.id),a.event_key,l.agent_id,l.device_id,l.source,l.source_id,l.file_identity,a.host,a.event_id,
	min(l.event_at),max(l.event_at),min(l.received_at),max(l.received_at),count(*),
	array_agg(DISTINCT a.record_type ORDER BY a.record_type),
	bool_or(a.success IS TRUE),bool_or(a.success IS FALSE)` + logJoin + where +
		` GROUP BY a.event_key,l.agent_id,l.device_id,l.source,l.source_id,l.file_identity,a.host,a.event_id` + having + fmt.Sprintf(" ORDER BY max(l.id) DESC LIMIT $%d", len(args))
	out := AuditPage{Items: []logbook.AuditEvent{}, Limit: f.Limit}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var e logbook.AuditEvent
		var yes, no bool
		if err = rows.Scan(&e.CursorID, &e.Key, &e.AgentID, &e.DeviceID, &e.Source, &e.SourceID, &e.FileIdentity, &e.Host, &e.EventID, &e.FirstEventAt, &e.LastEventAt, &e.FirstReceivedAt, &e.LastReceivedAt, &e.RecordCount, &e.RecordTypes, &yes, &no); err != nil {
			return out, err
		}
		e.FirstEventAt, e.LastEventAt, e.FirstReceivedAt, e.LastReceivedAt = e.FirstEventAt.UTC(), e.LastEventAt.UTC(), e.FirstReceivedAt.UTC(), e.LastReceivedAt.UTC()
		e.Outcome = "unknown"
		if yes && no {
			e.Outcome = "mixed"
		} else if yes {
			e.Outcome = "success"
		} else if no {
			e.Outcome = "failure"
		}
		e.Completeness = "unknown"
		out.Items = append(out.Items, e)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if len(out.Items) > f.Limit {
		out.Items = out.Items[:f.Limit]
		id := out.Items[len(out.Items)-1].CursorID
		out.NextBeforeID = &id
	}
	return out, nil
}
