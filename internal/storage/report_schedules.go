package storage

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/reports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"strconv"
	"strings"
	"time"
)

const reportScheduleColumns = `id,revision,owner_id,definition,next_run_at,last_snapshot_id,failure,updated_at`

func readReportSchedule(row interface{ Scan(...any) error }) (reports.Schedule, error) {
	var v reports.Schedule
	var raw []byte
	err := row.Scan(&v.ID, &v.Revision, &v.OwnerID, &raw, &v.NextRunAt, &v.LastSnapshotID, &v.Failure, &v.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, reports.ErrNotFound
	}
	if err != nil {
		return v, err
	}
	err = json.Unmarshal(raw, &v.Definition)
	return v, err
}
func (s *Store) SaveReportSchedule(ctx context.Context, id string, input reports.ScheduleInput) (reports.Schedule, error) {
	var empty reports.Schedule
	if input.Validate() != nil {
		return empty, reports.ErrInvalid
	}
	actor, ok := access.Current(ctx)
	if !ok || actor.User.Role != "admin" {
		return empty, access.ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(context.Background())
	if _, err = automationActorTx(ctx, tx, actor, true); err != nil {
		return empty, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "report-schedule:"+actor.User.ID); err != nil {
		return empty, err
	}
	if id == "" {
		var owned int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM report_schedules WHERE owner_id=$1`, actor.User.ID).Scan(&owned); err != nil {
			return empty, err
		}
		if owned >= 20 {
			return empty, reports.ErrConflict
		}
		if input.ExpectedRevision != 0 {
			return empty, reports.ErrConflict
		}
		id, err = aiID("rsc_")
		if err != nil {
			return empty, err
		}
	} else {
		old, e := readReportSchedule(tx.QueryRow(ctx, `SELECT `+reportScheduleColumns+` FROM report_schedules WHERE id=$1 FOR UPDATE`, id))
		if e != nil {
			return empty, e
		}
		if old.Revision != input.ExpectedRevision {
			return empty, reports.ErrConflict
		}
	}
	// Validate scope before persisting a durable background authorization.
	for _, entry := range []struct{ id, query string }{{input.AssetID, `SELECT EXISTS(SELECT 1 FROM assets WHERE id=$1 AND lifecycle_status='observed')`}, {input.SiteID, `SELECT EXISTS(SELECT 1 FROM physical_locations WHERE id=$1 AND kind='site')`}} {
		if entry.id == "" {
			continue
		}
		var exists bool
		if err = tx.QueryRow(ctx, entry.query, entry.id).Scan(&exists); err != nil {
			return empty, err
		}
		if !exists {
			return empty, ErrNotFound
		}
	}
	if input.Scope == "" {
		input.Scope = "real"
	}
	input.Name = strings.TrimSpace(logbook.SanitizeText(input.Name))
	input.Reason = logbook.SanitizeText(input.Reason)
	encoded, _ := json.Marshal(input)
	v, err := readReportSchedule(tx.QueryRow(ctx, `INSERT INTO report_schedules(id,revision,owner_id,enabled,definition,next_run_at) VALUES($1,$2,$3,$4,$5,now()) ON CONFLICT(id) DO UPDATE SET revision=excluded.revision,owner_id=excluded.owner_id,enabled=excluded.enabled,definition=excluded.definition,next_run_at=now(),failure='',updated_at=now() RETURNING `+reportScheduleColumns, id, input.ExpectedRevision+1, actor.User.ID, input.Enabled, encoded))
	if err != nil {
		var violation *pgconn.PgError
		if errors.As(err, &violation) && violation.Code == "23505" {
			return empty, reports.ErrConflict
		}
		return empty, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO report_schedule_events(schedule_id,actor_id,revision,definition) VALUES($1,$2,$3,$4)`, id, actor.User.ID, v.Revision, encoded); err != nil {
		return empty, err
	}
	meta := access.CurrentMeta(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,'report_schedule_saved','success',$2,$3,$4)`, actor.User.ID, "schedule:"+id, meta.RequestID, meta.Peer); err != nil {
		return empty, err
	}
	return v, tx.Commit(ctx)
}
func (s *Store) ReportSchedules(ctx context.Context, limit, offset int) ([]reports.Schedule, error) {
	if limit < 1 || limit > 100 || offset < 0 || offset > 100000 {
		return nil, reports.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT `+reportScheduleColumns+` FROM report_schedules ORDER BY updated_at DESC,id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []reports.Schedule{}
	for rows.Next() {
		v, e := readReportSchedule(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) ProcessScheduledReports(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	schedule, err := readReportSchedule(tx.QueryRow(ctx, `SELECT `+reportScheduleColumns+` FROM report_schedules WHERE enabled AND next_run_at<=$1 ORDER BY next_run_at,id LIMIT 1`, now))
	if errors.Is(err, reports.ErrNotFound) {
		return 0, tx.Commit(ctx)
	}
	if err != nil {
		return 0, err
	}
	var owner, role string
	var disabled bool
	err = tx.QueryRow(ctx, `SELECT id,role,disabled FROM access_users WHERE id=$1 FOR SHARE`, schedule.OwnerID).Scan(&owner, &role, &disabled)
	if err != nil {
		return 0, err
	}
	locked, e := readReportSchedule(tx.QueryRow(ctx, `SELECT `+reportScheduleColumns+` FROM report_schedules WHERE id=$1 AND enabled AND next_run_at<=$2 FOR UPDATE SKIP LOCKED`, schedule.ID, now))
	if errors.Is(e, reports.ErrNotFound) {
		return 0, tx.Commit(ctx)
	}
	if e != nil {
		return 0, e
	}
	if locked.OwnerID != schedule.OwnerID || locked.Revision != schedule.Revision {
		return 0, tx.Commit(ctx)
	}
	schedule = locked
	if role != "admin" || disabled {
		if _, err = tx.Exec(ctx, `UPDATE report_schedules SET enabled=false,definition=jsonb_set(definition,'{enabled}','false'::jsonb),failure='owner_access_revoked',updated_at=now() WHERE id=$1`, schedule.ID); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,'report_schedule_blocked','denied',$2,'scheduler','internal')`, schedule.OwnerID, "schedule:"+schedule.ID); err != nil {
			return 0, err
		}
		return 0, tx.Commit(ctx)
	}
	if err != nil {
		return 0, err
	}
	from, to, next := reports.ClosedPeriod(now, schedule.Definition.Cadence)
	f, err := (reports.V2Filter{From: from, To: to, AssetID: schedule.Definition.AssetID, SiteID: schedule.Definition.SiteID, Scope: schedule.Definition.Scope, Limit: 100}).Normalize(time.Now().UTC())
	if err != nil {
		return 0, err
	}
	var existing string
	err = tx.QueryRow(ctx, `SELECT snapshot_id FROM report_schedule_runs WHERE schedule_id=$1 AND revision=$2 AND period_to=$3`, schedule.ID, schedule.Revision, to).Scan(&existing)
	if err == nil {
		if _, err = tx.Exec(ctx, `UPDATE report_schedules SET next_run_at=$2,last_snapshot_id=$3 WHERE id=$1`, schedule.ID, next, existing); err != nil {
			return 0, err
		}
		return 0, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	summary, err := s.reportV2SummaryTx(ctx, tx, f)
	if errors.Is(err, ErrNotFound) {
		if _, err = tx.Exec(ctx, `UPDATE report_schedules SET enabled=false,definition=jsonb_set(definition,'{enabled}','false'::jsonb),failure='scope_unavailable',updated_at=now() WHERE id=$1`, schedule.ID); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,'report_schedule_blocked','denied',$2,'scheduler','internal')`, owner, "schedule:"+schedule.ID); err != nil {
			return 0, err
		}
		return 0, tx.Commit(ctx)
	}
	if err != nil {
		return 0, err
	}
	key := "schedule:" + schedule.ID + ":" + strconv.FormatInt(schedule.Revision, 10) + ":" + to.Format(time.RFC3339)
	snapshot, err := insertReportV2Snapshot(ctx, tx, owner, key, f, summary)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO report_schedule_runs(schedule_id,revision,period_to,snapshot_id) VALUES($1,$2,$3,$4)`, schedule.ID, schedule.Revision, to, snapshot.ID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE report_schedules SET next_run_at=$2,last_snapshot_id=$3,failure='',updated_at=now() WHERE id=$1`, schedule.ID, next, snapshot.ID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,'report_schedule_generated','success',$2,'scheduler','internal')`, owner, "report:"+snapshot.ID); err != nil {
		return 0, err
	}
	return 1, tx.Commit(ctx)
}
