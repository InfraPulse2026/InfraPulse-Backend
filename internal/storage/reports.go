package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/reports"
	"github.com/jackc/pgx/v5"
)

const reportBase = `WITH matched AS(SELECT i.*,GREATEST(0,EXTRACT(epoch FROM LEAST(COALESCE(resolved_at,$2),$2)-GREATEST(opened_at,$1)))::double precision AS clipped_seconds FROM incidents i
 WHERE opened_at<$2 AND COALESCE(resolved_at,'infinity'::timestamptz)>$1 AND ($3='' OR agent_id=$3) AND ($4='' OR source=$4) AND ($5='' OR resource_kind=$5)) `

func (s *Store) ReportSummary(ctx context.Context, filter reports.Filter) (reports.Summary, error) {
	var report reports.Summary
	f, err := filter.Normalize(time.Now())
	if err != nil {
		return report, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return report, err
	}
	defer tx.Rollback(context.Background())
	report.Version = "1"
	report.Filter = f
	report.Methodology = reports.Methodology()
	report.ByRule = []reports.RuleGroup{}
	report.TopResources = []reports.ResourceGroup{}
	report.Recurring = []reports.Recurrence{}
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&report.GeneratedAt); err != nil {
		return report, err
	}
	args := []any{f.From, f.To, f.AgentID, f.Source, f.ResourceKind}
	err = tx.QueryRow(ctx, reportBase+`SELECT count(*),count(*) FILTER(WHERE opened_at>=$1),count(*) FILTER(WHERE opened_at<$1),count(*) FILTER(WHERE resolved_at>=$1 AND resolved_at<$2),count(*) FILTER(WHERE resolved_at IS NULL OR resolved_at>=$2),count(*) FILTER(WHERE acknowledged_at>=$1 AND acknowledged_at<$2),COALESCE(sum(clipped_seconds),0),avg(EXTRACT(epoch FROM acknowledged_at-opened_at)) FILTER(WHERE acknowledged_at>=$1 AND acknowledged_at<$2 AND acknowledged_at>=opened_at),avg(EXTRACT(epoch FROM resolved_at-opened_at)) FILTER(WHERE resolved_at>=$1 AND resolved_at<$2 AND resolved_at>=opened_at),count(*) FILTER(WHERE source<>'simulated' AND scope=''),count(*) FILTER(WHERE source='simulated' OR scope<>'') FROM matched`, args...).Scan(&report.Stats.Overlapping, &report.Stats.Opened, &report.Stats.CarriedIn, &report.Stats.Resolved, &report.Stats.ActiveAtEnd, &report.Stats.Acknowledged, &report.Stats.SummedSeconds, &report.Stats.MeanAckSeconds, &report.Stats.MeanResolutionSeconds, &report.Stats.RealEpisodes, &report.Stats.SimulatedEpisodes)
	if err != nil {
		return report, err
	}
	rows, err := tx.Query(ctx, reportBase+`SELECT rule_id,source,(source='simulated' OR scope<>'') AS simulated,count(*),count(*) FILTER(WHERE opened_at>=$1),sum(clipped_seconds) FROM matched GROUP BY rule_id,source,simulated ORDER BY count(*) DESC,rule_id,source LIMIT 1000`, args...)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var v reports.RuleGroup
		if err = rows.Scan(&v.RuleID, &v.Source, &v.Simulated, &v.Count, &v.Opened, &v.Seconds); err != nil {
			rows.Close()
			return report, err
		}
		report.ByRule = append(report.ByRule, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return report, err
	}
	report.RuleGroupsTruncated = len(report.ByRule) >= 1000
	topArgs := append(append([]any{}, args...), f.TopLimit)
	rows, err = tx.Query(ctx, reportBase+`SELECT COALESCE(agent_id,''),COALESCE(device_id,''),resource_kind,resource_id,resource_context,source,scope,count(*),sum(clipped_seconds) FROM matched GROUP BY agent_id,device_id,resource_kind,resource_id,resource_context,source,scope ORDER BY sum(clipped_seconds) DESC,count(*) DESC,agent_id,COALESCE(device_id,''),resource_kind,resource_id,resource_context,source,scope LIMIT $6`, topArgs...)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var v reports.ResourceGroup
		if err = rows.Scan(&v.AgentID, &v.DeviceID, &v.ResourceKind, &v.ResourceID, &v.ResourceContext, &v.Source, &v.Scope, &v.Count, &v.Seconds); err != nil {
			rows.Close()
			return report, err
		}
		report.TopResources = append(report.TopResources, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return report, err
	}
	rows, err = tx.Query(ctx, reportBase+`SELECT COALESCE(agent_id,''),COALESCE(device_id,''),resource_kind,resource_id,resource_context,source,scope,count(*),sum(clipped_seconds),rule_id,count(*) FILTER(WHERE opened_at>=$1),min(opened_at) FILTER(WHERE opened_at>=$1),max(opened_at) FILTER(WHERE opened_at>=$1) FROM matched GROUP BY agent_id,device_id,resource_kind,resource_id,resource_context,source,scope,rule_id HAVING count(*) FILTER(WHERE opened_at>=$1)>=2 ORDER BY count(*) FILTER(WHERE opened_at>=$1) DESC,sum(clipped_seconds) DESC,agent_id,COALESCE(device_id,''),resource_kind,resource_id,resource_context,source,scope,rule_id LIMIT $6`, topArgs...)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var v reports.Recurrence
		if err = rows.Scan(&v.AgentID, &v.DeviceID, &v.ResourceKind, &v.ResourceID, &v.ResourceContext, &v.Source, &v.Scope, &v.Count, &v.Seconds, &v.RuleID, &v.OpenedOccurrences, &v.FirstOpened, &v.LastOpened); err != nil {
			rows.Close()
			return report, err
		}
		report.Recurring = append(report.Recurring, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return report, err
	}
	err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE cpu_usage_percent IS NULL),count(*) FILTER(WHERE observed_at>received_at+interval '30 seconds'),count(*) FILTER(WHERE received_at>observed_at+interval '5 minutes') FROM telemetry WHERE received_at>=$1 AND received_at<$2 AND ($3='' OR agent_id=$3) AND ($4='' OR source=$4)`, f.From, f.To, f.AgentID, f.Source).Scan(&report.Quality.TelemetryReceived, &report.Quality.MissingCPU, &report.Quality.FutureClock, &report.Quality.LateSamples)
	if err != nil {
		return report, err
	}
	err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE latency_ms IS NULL) FROM telephony_samples WHERE received_at>=$1 AND received_at<$2 AND ($3='' OR agent_id=$3) AND ($4='' OR source=$4)`, f.From, f.To, f.AgentID, f.Source).Scan(&report.Quality.SIPReceived, &report.Quality.SIPMissingLatency)
	if err != nil {
		return report, err
	}
	report.Quality.CoverageStatus = "received_samples_only_not_uptime"
	report.Quality.FilterScope = "agent_and_source; resource_kind does not restrict raw sample quality counts"
	return report, tx.Commit(ctx)
}

func readReportSnapshot(row interface{ Scan(...any) error }, include bool) (reports.Snapshot, error) {
	var v reports.Snapshot
	var filter, raw []byte
	err := row.Scan(&v.ID, &v.CreatedAt, &v.CreatedBy, &filter, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, reports.ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal(filter, &v.Filter); err != nil {
		return v, err
	}
	if include {
		v.Report = &reports.Summary{}
		err = json.Unmarshal(raw, v.Report)
	}
	return v, err
}
func (s *Store) CreateReportSnapshot(ctx context.Context, f reports.Filter, key string) (reports.Snapshot, bool, error) {
	var empty reports.Snapshot
	actor, ok := access.Current(ctx)
	if !ok || !access.Allowed(actor.User.Role, access.ReportWrite) {
		return empty, false, access.ErrForbidden
	}
	if err := commands.ValidateKey(key); err != nil {
		return empty, false, reports.ErrInvalid
	}
	normalized, err := f.Normalize(time.Now())
	if err != nil {
		return empty, false, err
	}
	f = normalized
	// The report itself is one read-only snapshot. The following short write
	// transaction serializes idempotency, rechecks the actor and stores/audits it.
	current, err := s.AccessSession(ctx, actor.SessionHash)
	if err != nil {
		return empty, false, err
	}
	if current.User.ID != actor.User.ID || !access.Allowed(current.User.Role, access.ReportWrite) {
		return empty, false, access.ErrForbidden
	}
	report, err := s.ReportSummary(ctx, f)
	if err != nil {
		return empty, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, false, err
	}
	defer tx.Rollback(context.Background())
	if err = commandActor(ctx, tx, actor.User.ID, actor.SessionHash); err != nil {
		return empty, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "report:"+actor.User.ID+":"+key); err != nil {
		return empty, false, err
	}
	var fingerprint string
	err = tx.QueryRow(ctx, `SELECT request_fingerprint FROM report_snapshots WHERE created_by=$1 AND idempotency_key=$2`, actor.User.ID, key).Scan(&fingerprint)
	if err == nil {
		if fingerprint != f.Fingerprint() {
			return empty, false, reports.ErrConflict
		}
		v, e := readReportSnapshot(tx.QueryRow(ctx, `SELECT id,created_at,created_by,filter,report FROM report_snapshots WHERE created_by=$1 AND idempotency_key=$2`, actor.User.ID, key), true)
		if e != nil {
			return empty, false, e
		}
		return v, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, false, err
	}
	idBytes := make([]byte, 16)
	if _, err = rand.Read(idBytes); err != nil {
		return empty, false, err
	}
	id := "rpt_" + hex.EncodeToString(idBytes)
	filterJSON, _ := json.Marshal(f)
	reportJSON, err := json.Marshal(report)
	if err != nil {
		return empty, false, err
	}
	v, err := readReportSnapshot(tx.QueryRow(ctx, `INSERT INTO report_snapshots(id,created_by,idempotency_key,request_fingerprint,filter,report) VALUES($1,$2,$3,$4,$5,$6) RETURNING id,created_at,created_by,filter,report`, id, actor.User.ID, key, f.Fingerprint(), filterJSON, reportJSON), true)
	if err != nil {
		return empty, false, err
	}
	meta := access.CurrentMeta(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,'report_snapshot_created','success',$2,$3,$4)`, actor.User.ID, "report:"+id, meta.RequestID, meta.Peer); err != nil {
		return empty, false, err
	}
	return v, false, tx.Commit(ctx)
}
func (s *Store) ReportSnapshot(ctx context.Context, id string) (reports.Snapshot, error) {
	return readReportSnapshot(s.pool.QueryRow(ctx, `SELECT id,created_at,created_by,filter,report FROM report_snapshots WHERE id=$1 AND kind='legacy'`, id), true)
}
func (s *Store) ReportSnapshots(ctx context.Context, limit, offset int) ([]reports.Snapshot, error) {
	if limit < 1 || limit > 100 || offset < 0 || offset > 100000 {
		return nil, reports.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id,created_at,created_by,filter,NULL::jsonb FROM report_snapshots WHERE kind='legacy' ORDER BY created_at DESC,id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []reports.Snapshot{}
	for rows.Next() {
		v, e := readReportSnapshot(rows, false)
		if e != nil {
			return nil, e
		}
		items = append(items, v)
	}
	return items, rows.Err()
}
