package storage

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assetlogs"
	"github.com/jackc/pgx/v5"
)

// Associate only an existing inventory binding owned by the reporting
// Collector and matching the record's exact device ID and simulation session.
// A log-only placeholder device, a claimed hostname, peer IP, SourceRef ID or
// TLS identity cannot create or select a binding through this query.
// Keep historical rows and audit keys intact while resolving typed simulation
// using the immutable envelope session (older projections left record_scope empty).
const assetLogScopeJoin = ` LEFT JOIN agent_messages lm ON lm.agent_id=l.agent_id AND lm.message_id=l.message_id
 CROSS JOIN LATERAL (SELECT CASE WHEN l.source='simulated'
 OR l.observation_metadata->'source'->>'provenance'='simulated'
 OR lm.payload->'logs'->>'source'='simulated'
 OR lm.payload->'logs'->'observation'->'source'->>'provenance'='simulated'
 THEN COALESCE(NULLIF(l.record_scope,''),lm.session_id) ELSE l.record_scope END AS record_scope) ls `

const assetLogJoin = logJoin + assetLogScopeJoin + ` JOIN data_sources ds ON ds.type='COLLECTOR' AND ds.connection_ref=l.agent_id
 JOIN asset_source_bindings b ON b.source_instance_id=ds.id AND b.external_type='device' AND b.external_id=l.device_id AND b.scope=ls.record_scope AND b.legacy_agent_id=l.agent_id AND b.legacy_device_id=l.device_id `

func assetLogWhere(f assetlogs.Filter, asset string, cursor bool) (string, []any) {
	where, args := logWhere(f.Log, cursor)
	add := func(expression string, value any) {
		args = append(args, value)
		where += fmt.Sprintf(" AND "+expression, len(args))
	}
	add("b.asset_id=$%d", asset)
	if f.SourceInstanceID != "" {
		add("b.source_instance_id=$%d", f.SourceInstanceID)
	}
	if f.RecordScope != nil {
		add("ls.record_scope=$%d", *f.RecordScope)
	}
	return where, args
}

func assetLogExists(ctx context.Context, tx pgx.Tx, id string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM assets WHERE id=$1)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

// extraLogScanner lets the legacy scanner remain unchanged while adding source
// binding metadata to a single SQL row. No persisted historical Asset ID is
// invented; all queries use the current binding after merges or splits.
type extraLogScanner struct {
	row   interface{ Scan(...any) error }
	extra []any
}

func (s extraLogScanner) Scan(values ...any) error {
	return s.row.Scan(append(values, s.extra...)...)
}

func scanAssetLog(row interface{ Scan(...any) error }, asset string) (assetlogs.Record, error) {
	var binding, source, scope string
	r, err := scanLog(extraLogScanner{row: row, extra: []any{&binding, &source, &scope}})
	return assetlogs.Public(r, asset, binding, source, scope), err
}

func (s *Store) AssetLogs(ctx context.Context, asset string, f assetlogs.Filter) (assetlogs.Page, error) {
	out := assetlogs.Page{AssetID: asset, Items: []assetlogs.Record{}, Limit: f.Log.Limit}
	if !assetlogs.ValidAssetID(asset) || assetlogs.ValidateFilter(f) != nil {
		return out, assetlogs.ErrFilter
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	if err = assetLogExists(ctx, tx, asset); err != nil {
		return out, err
	}
	where, args := assetLogWhere(f, asset, true)
	args = append(args, f.Log.Limit+1)
	rows, err := tx.Query(ctx, `SELECT `+logColumns+`,b.id,b.source_instance_id,ls.record_scope`+assetLogJoin+where+fmt.Sprintf(` ORDER BY l.id DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		r, scanErr := scanAssetLog(rows, asset)
		if scanErr != nil {
			rows.Close()
			return out, scanErr
		}
		out.Items = append(out.Items, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Items) > f.Log.Limit {
		out.Items = out.Items[:f.Log.Limit]
		id := out.Items[len(out.Items)-1].ID
		out.NextBeforeID = &id
	}
	return out, tx.Commit(ctx)
}

func (s *Store) AssetLog(ctx context.Context, asset string, id int64) (assetlogs.Record, error) {
	if !assetlogs.ValidAssetID(asset) || id < 1 {
		return assetlogs.Record{}, assetlogs.ErrFilter
	}
	r, err := scanAssetLog(s.pool.QueryRow(ctx, `SELECT `+logColumns+`,b.id,b.source_instance_id,ls.record_scope`+assetLogJoin+` WHERE b.asset_id=$1 AND l.id=$2`, asset, id), asset)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

func (s *Store) AssetAuditEvents(ctx context.Context, asset string, f assetlogs.Filter) (assetlogs.AuditPage, error) {
	f.Log.AuditOnly = true
	out := assetlogs.AuditPage{AssetID: asset, Items: []assetlogs.AuditEvent{}, Limit: f.Log.Limit}
	if !assetlogs.ValidAssetID(asset) || assetlogs.ValidateFilter(f) != nil {
		return out, assetlogs.ErrFilter
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	if err = assetLogExists(ctx, tx, asset); err != nil {
		return out, err
	}
	// Match events using individual record filters, then aggregate the entire
	// selected event. A SYSCALL/PATH or text filter must not change group count,
	// outcome or record types by silently dropping other records of the event.
	where, args := assetLogWhere(f, asset, false)
	assetPos := len(args) + 1
	args = append(args, asset)
	having := ""
	if f.Log.BeforeID > 0 {
		args = append(args, f.Log.BeforeID)
		having = fmt.Sprintf(` HAVING max(l.id)<$%d`, len(args))
	}
	args = append(args, f.Log.Limit+1)
	query := `WITH matched AS (SELECT DISTINCT a.event_key` + assetLogJoin + where + `)
 SELECT max(l.id),a.event_key,l.agent_id,l.device_id,l.source,l.source_id,l.file_identity,a.host,a.event_id,
 min(l.event_at),max(l.event_at),min(l.received_at),max(l.received_at),count(*),array_agg(DISTINCT a.record_type ORDER BY a.record_type),bool_or(a.success IS TRUE),bool_or(a.success IS FALSE),b.id,b.source_instance_id,ls.record_scope` + assetLogJoin + ` JOIN matched m ON m.event_key=a.event_key WHERE b.asset_id=$` + strconv.Itoa(assetPos) + ` GROUP BY a.event_key,l.agent_id,l.device_id,l.source,l.source_id,l.file_identity,a.host,a.event_id,b.id,b.source_instance_id,ls.record_scope` + having + fmt.Sprintf(` ORDER BY max(l.id) DESC LIMIT $%d`, len(args))
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var e assetlogs.AuditEvent
		var yes, no bool
		if err = rows.Scan(&e.CursorID, &e.Key, &e.AgentID, &e.DeviceID, &e.Source, &e.SourceID, &e.FileIdentity, &e.Host, &e.EventID, &e.FirstEventAt, &e.LastEventAt, &e.FirstReceivedAt, &e.LastReceivedAt, &e.RecordCount, &e.RecordTypes, &yes, &no, &e.BindingID, &e.SourceInstanceID, &e.RecordScope); err != nil {
			rows.Close()
			return out, err
		}
		e.AssetID, e.Association, e.Completeness = asset, "source_owned_device_reference", "unknown"
		e.FirstEventAt, e.LastEventAt, e.FirstReceivedAt, e.LastReceivedAt = e.FirstEventAt.UTC(), e.LastEventAt.UTC(), e.FirstReceivedAt.UTC(), e.LastReceivedAt.UTC()
		e.Outcome = "unknown"
		if yes && no {
			e.Outcome = "mixed"
		} else if yes {
			e.Outcome = "success"
		} else if no {
			e.Outcome = "failure"
		}
		out.Items = append(out.Items, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Items) > f.Log.Limit {
		out.Items = out.Items[:f.Log.Limit]
		id := out.Items[len(out.Items)-1].CursorID
		out.NextBeforeID = &id
	}
	return out, tx.Commit(ctx)
}

func (s *Store) AssetLogRaw(ctx context.Context, asset string, id int64) (assetlogs.Raw, error) {
	return s.logRaw(ctx, asset, id, "", true)
}

// LogRaw also permits an administrator to inspect retained, redacted raw text
// for unresolved sender records that have no inventory binding. It never
// guesses an Asset from claimed hostnames or transport addresses.
func (s *Store) LogRaw(ctx context.Context, id int64, agent string) (assetlogs.Raw, error) {
	return s.logRaw(ctx, "", id, agent, false)
}

func (s *Store) logRaw(ctx context.Context, asset string, id int64, agent string, scoped bool) (assetlogs.Raw, error) {
	out := assetlogs.Raw{AssetID: asset, ID: id, ArchivePayloadNotExposed: true}
	p, ok := access.Current(ctx)
	if !ok || p.User.Role != "admin" {
		return out, access.ErrForbidden
	}
	if scoped && !assetlogs.ValidAssetID(asset) || id < 1 || len(agent) > 256 {
		return out, assetlogs.ErrFilter
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	// User role/session changes take the exclusive form of this lock. Shared
	// readers revalidate current authority and finish the audit before revocation.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(73194002)`); err != nil {
		return out, err
	}
	var actor string
	err = tx.QueryRow(ctx, `SELECT u.id FROM access_users u JOIN access_sessions sess ON sess.user_id=u.id WHERE u.id=$1 AND u.role='admin' AND NOT u.disabled AND sess.token_hash=$2 AND sess.expires_at>now() FOR SHARE OF u,sess`, p.User.ID, p.SessionHash).Scan(&actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, access.ErrForbidden
	}
	if err != nil {
		return out, err
	}
	if scoped {
		err = tx.QueryRow(ctx, `SELECT b.id,b.source_instance_id,ls.record_scope,l.raw,l.redacted,l.truncated,l.raw_payload_omitted`+assetLogJoin+` WHERE b.asset_id=$1 AND l.id=$2`, asset, id).Scan(&out.BindingID, &out.SourceInstanceID, &out.RecordScope, &out.Raw, &out.Redacted, &out.Truncated, &out.RawPayloadAvailable)
	} else {
		err = tx.QueryRow(ctx, `SELECT COALESCE(b.asset_id,''),COALESCE(b.id,''),COALESCE(ds.id,''),ls.record_scope,l.raw,l.redacted,l.truncated,l.raw_payload_omitted FROM log_records l`+assetLogScopeJoin+`
 LEFT JOIN data_sources ds ON ds.type='COLLECTOR' AND ds.connection_ref=l.agent_id
 LEFT JOIN asset_source_bindings b ON b.source_instance_id=ds.id AND b.external_type='device' AND b.external_id=l.device_id AND b.scope=ls.record_scope AND b.legacy_agent_id=l.agent_id AND b.legacy_device_id=l.device_id
 WHERE l.id=$1 AND ($2='' OR l.agent_id=$2)`, id, agent).Scan(&out.AssetID, &out.BindingID, &out.SourceInstanceID, &out.RecordScope, &out.Raw, &out.Redacted, &out.Truncated, &out.RawPayloadAvailable)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return assetlogs.Raw{}, ErrNotFound
	}
	if err != nil {
		return assetlogs.Raw{}, err
	}
	if len(out.Raw) > assetlogs.MaxRawBytes {
		return assetlogs.Raw{}, assetlogs.ErrFilter
	}
	meta := access.CurrentMeta(ctx)
	resource := "log:" + strconv.FormatInt(id, 10)
	action := "log_raw_read"
	if scoped {
		resource, action = "asset:"+asset+"/"+resource, "asset_log_raw_read"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,$2,'success',$3,$4,$5)`, p.User.ID, action, resource, meta.RequestID, meta.Peer); err != nil {
		return assetlogs.Raw{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return assetlogs.Raw{}, err
	}
	return out, nil
}
