package storage

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/reports"
	"github.com/jackc/pgx/v5"
	"time"
)

const reportV2Base = `WITH RECURSIVE located(asset_id,id,parent_id,kind,depth) AS (
 SELECT ap.asset_id,l.id,l.parent_id,l.kind,1 FROM asset_placements ap JOIN physical_locations l ON l.id=ap.location_id
 UNION ALL SELECT p.asset_id,l.id,l.parent_id,l.kind,p.depth+1 FROM located p JOIN physical_locations l ON l.id=p.parent_id WHERE p.depth<8
), eligible AS (SELECT a.id FROM assets a WHERE a.lifecycle_status='observed' AND a.resolution_status<>'conflict' AND ($3='' OR a.id=$3) AND ($4='' OR EXISTS(SELECT 1 FROM located p WHERE p.asset_id=a.id AND p.id=$4 AND p.kind='site'))), matched AS (
 SELECT DISTINCT n.id AS signal_id,n.category,n.source_kind,n.source_instance_id,n.scope,CASE WHEN n.scope<>'' THEN COALESCE(n.payload->>'agent_id','') ELSE '' END AS simulation_collector,a.asset_id,i.rule_id,i.opened_at,i.resolved_at,c.id AS case_id,c.opened_at AS case_opened,c.resolved_at AS case_resolved
 FROM normalized_signals n JOIN incidents i ON i.id=n.incident_id JOIN normalized_signal_assets a ON a.signal_id=n.id JOIN eligible e ON e.id=a.asset_id JOIN correlated_incident_signals m ON m.signal_id=n.id JOIN correlated_incidents c ON c.id=m.correlated_incident_id
 WHERE i.opened_at<$2 AND COALESCE(i.resolved_at,'infinity'::timestamptz)>$1 AND c.opened_at<$2 AND COALESCE(c.resolved_at,'infinity'::timestamptz)>$1
 AND ($5='all' OR ($5='real' AND n.scope='' AND i.source<>'simulated') OR ($5='simulated' AND (n.scope<>'' OR i.source='simulated')))
) `

func (s *Store) ReportV2Summary(ctx context.Context, input reports.V2Filter) (reports.V2Summary, error) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ctx = bounded
	f, err := input.Normalize(time.Now().UTC())
	if err != nil {
		return reports.V2Summary{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return reports.V2Summary{}, err
	}
	defer tx.Rollback(context.Background())
	result, err := s.reportV2SummaryTx(ctx, tx, f)
	if err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}
func (s *Store) reportV2SummaryTx(ctx context.Context, tx pgx.Tx, f reports.V2Filter) (reports.V2Summary, error) {
	out := reports.V2Summary{Version: "2", Filter: f, Categories: []reports.CategoryGroup{}, Chronic: []reports.ChronicProblem{}, Sources: []reports.SourceGroup{}, Methodology: reports.V2Methodology()}
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&out.GeneratedAt); err != nil {
		return out, err
	}
	for _, entry := range []struct{ id, query string }{{f.AssetID, `SELECT EXISTS(SELECT 1 FROM assets WHERE id=$1 AND lifecycle_status='observed')`}, {f.SiteID, `SELECT EXISTS(SELECT 1 FROM physical_locations WHERE id=$1 AND kind='site')`}} {
		if entry.id == "" {
			continue
		}
		var exists bool
		if err := tx.QueryRow(ctx, entry.query, entry.id).Scan(&exists); err != nil {
			return out, err
		}
		if !exists {
			return out, ErrNotFound
		}
	}
	args := []any{f.From, f.To, f.AssetID, f.SiteID, f.Scope}
	err := tx.QueryRow(ctx, reportV2Base+`SELECT count(DISTINCT case_id),count(DISTINCT signal_id),count(DISTINCT asset_id),count(DISTINCT case_id) FILTER(WHERE case_resolved IS NULL OR case_resolved>=$2),(SELECT COALESCE(sum(GREATEST(0,EXTRACT(epoch FROM LEAST(COALESCE(case_resolved,$2),$2)-GREATEST(case_opened,$1)))),0)::double precision FROM (SELECT DISTINCT case_id,case_opened,case_resolved FROM matched) periods) FROM matched`, args...).Scan(&out.Stats.Cases, &out.Stats.Signals, &out.Stats.Assets, &out.Stats.ActiveAtEnd, &out.Stats.SummedSeconds)
	if err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, reportV2Base+`SELECT category,count(DISTINCT case_id),count(DISTINCT signal_id) FROM matched GROUP BY category ORDER BY category`, args...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var v reports.CategoryGroup
		if err = rows.Scan(&v.Category, &v.Cases, &v.Signals); err != nil {
			rows.Close()
			return out, err
		}
		out.Categories = append(out.Categories, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	bounded := append(append([]any{}, args...), f.Limit)
	rows, err = tx.Query(ctx, reportV2Base+`SELECT asset_id,category,scope,simulation_collector,count(DISTINCT case_id),count(DISTINCT signal_id),count(DISTINCT(source_kind,source_instance_id)),min(case_opened),max(case_opened),array_agg(DISTINCT rule_id ORDER BY rule_id),count(*) OVER() FROM matched WHERE case_opened>=$1 GROUP BY asset_id,category,scope,simulation_collector HAVING count(DISTINCT case_id)>=3 ORDER BY count(DISTINCT case_id) DESC,asset_id,category,scope,simulation_collector LIMIT $6`, bounded...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var v reports.ChronicProblem
		if err = rows.Scan(&v.AssetID, &v.Category, &v.Scope, &v.SimulationCollector, &v.Cases, &v.Signals, &v.Sources, &v.FirstOpened, &v.LastOpened, &v.Rules, &out.ChronicTotal); err != nil {
			rows.Close()
			return out, err
		}
		v.RelatedServices = []string{}
		out.Chronic = append(out.Chronic, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	// Reviewed service IDs are current context; do not infer service outages.
	assetIDs := []string{}
	for _, v := range out.Chronic {
		assetIDs = append(assetIDs, v.AssetID)
	}
	if len(assetIDs) > 0 {
		rows, err = tx.Query(ctx, `SELECT a.asset_id,a.service_id FROM correlation_service_assets a JOIN correlation_services s ON s.id=a.service_id WHERE s.enabled AND a.asset_id=ANY($1::text[]) ORDER BY a.asset_id,a.service_id`, assetIDs)
		if err != nil {
			return out, err
		}
		services := map[string][]string{}
		for rows.Next() {
			var asset, service string
			if err = rows.Scan(&asset, &service); err != nil {
				rows.Close()
				return out, err
			}
			if len(services[asset]) < 50 {
				services[asset] = append(services[asset], service)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		for i := range out.Chronic {
			if ids := services[out.Chronic[i].AssetID]; ids != nil {
				out.Chronic[i].RelatedServices = ids
			}
		}
	}
	rows, err = tx.Query(ctx, reportV2Base+`SELECT groups.source_kind,groups.source_instance_id,groups.signals,d.enabled,d.connection_status,count(*) OVER() FROM (SELECT source_kind,source_instance_id,count(DISTINCT signal_id) AS signals FROM matched GROUP BY source_kind,source_instance_id) groups LEFT JOIN data_sources d ON d.id=groups.source_instance_id ORDER BY groups.source_kind,groups.source_instance_id LIMIT $6`, bounded...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var v reports.SourceGroup
		if err = rows.Scan(&v.Kind, &v.ID, &v.Signals, &v.Enabled, &v.Status, &out.SourcesTotal); err != nil {
			rows.Close()
			return out, err
		}
		out.Sources = append(out.Sources, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	out.Truncated = out.ChronicTotal > int64(len(out.Chronic)) || out.SourcesTotal > int64(len(out.Sources))
	if err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM correlation_pending)+(SELECT count(*) FROM hardware_signal_pending)+(SELECT count(*) FROM log_signal_pending)+(SELECT count(*) FROM configuration_signal_pending)+(SELECT count(*) FROM topology_signal_pending)`).Scan(&out.PendingNormalization); err != nil {
		return out, err
	}
	return out, nil
}

func readReportV2Snapshot(row interface{ Scan(...any) error }, include bool) (reports.V2Snapshot, error) {
	var v reports.V2Snapshot
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
		v.Report = &reports.V2Summary{}
		err = json.Unmarshal(raw, v.Report)
	}
	return v, err
}
func (s *Store) CreateReportV2Snapshot(ctx context.Context, input reports.V2Filter, key string) (reports.V2Snapshot, bool, error) {
	var empty reports.V2Snapshot
	actor, ok := access.Current(ctx)
	if !ok || !access.Allowed(actor.User.Role, access.ReportWrite) {
		return empty, false, access.ErrForbidden
	}
	if commands.ValidateKey(key) != nil {
		return empty, false, reports.ErrInvalid
	}
	f, err := input.Normalize(time.Now().UTC())
	if err != nil {
		return empty, false, err
	}
	report, err := s.ReportV2Summary(ctx, f)
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
	var hash, kind string
	err = tx.QueryRow(ctx, `SELECT request_fingerprint,kind FROM report_snapshots WHERE created_by=$1 AND idempotency_key=$2`, actor.User.ID, key).Scan(&hash, &kind)
	if err == nil {
		if hash != f.Fingerprint() || kind != "unified" {
			return empty, false, reports.ErrConflict
		}
		v, e := readReportV2Snapshot(tx.QueryRow(ctx, `SELECT id,created_at,created_by,filter,report FROM report_snapshots WHERE created_by=$1 AND idempotency_key=$2`, actor.User.ID, key), true)
		if e != nil {
			return empty, false, e
		}
		return v, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return empty, false, err
	}
	v, err := insertReportV2Snapshot(ctx, tx, actor.User.ID, key, f, report)
	if err != nil {
		return empty, false, err
	}
	meta := access.CurrentMeta(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,'report_v2_created','success',$2,$3,$4)`, actor.User.ID, "report:"+v.ID, meta.RequestID, meta.Peer); err != nil {
		return empty, false, err
	}
	return v, false, tx.Commit(ctx)
}
func insertReportV2Snapshot(ctx context.Context, tx pgx.Tx, actor, key string, f reports.V2Filter, report reports.V2Summary) (reports.V2Snapshot, error) {
	id, err := aiID("rpt_")
	if err != nil {
		return reports.V2Snapshot{}, err
	}
	filter, _ := json.Marshal(f)
	raw, err := json.Marshal(report)
	if err != nil {
		return reports.V2Snapshot{}, err
	}
	return readReportV2Snapshot(tx.QueryRow(ctx, `INSERT INTO report_snapshots(id,created_by,idempotency_key,request_fingerprint,filter,report,kind) VALUES($1,$2,$3,$4,$5,$6,'unified') RETURNING id,created_at,created_by,filter,report`, id, actor, key, f.Fingerprint(), filter, raw), true)
}
func (s *Store) ReportV2Snapshot(ctx context.Context, id string) (reports.V2Snapshot, error) {
	return readReportV2Snapshot(s.pool.QueryRow(ctx, `SELECT id,created_at,created_by,filter,report FROM report_snapshots WHERE id=$1 AND kind='unified'`, id), true)
}
func (s *Store) ReportV2Snapshots(ctx context.Context, limit, offset int) ([]reports.V2Snapshot, error) {
	if limit < 1 || limit > 100 || offset < 0 || offset > 100000 {
		return nil, reports.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id,created_at,created_by,filter,NULL::jsonb FROM report_snapshots WHERE kind='unified' ORDER BY created_at DESC,id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []reports.V2Snapshot{}
	for rows.Next() {
		v, e := readReportV2Snapshot(rows, false)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
