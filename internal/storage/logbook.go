package storage

import (
	"bytes"
	"context"
	"errors"
	"time"

	pb "github.com/InfraPulse2026/InfraPulse-Backend/api/gen/infrapulse/v1"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
)

var ErrLogConflict = errors.New("log record ID reused with different business contents")
var ErrInvalidLogs = errors.New("invalid log records")

func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func projectLogs(ctx context.Context, tx pgx.Tx, e *pb.AgentEnvelope, received time.Time) (int, error) {
	var localID string
	if err := tx.QueryRow(ctx, `SELECT local_device_id FROM agents WHERE agent_id=$1`, e.AgentId).Scan(&localID); err != nil {
		return 0, err
	}
	prepared, err := logbook.Prepare(e, localID, received)
	if err != nil {
		return 0, ErrInvalidLogs
	}
	batchAt := at(e.GetLogs().ObservedAt, e.SentAt.AsTime())
	result, err := tx.Exec(ctx, `INSERT INTO log_batches(agent_id,message_id,observed_at,received_at,source,record_count)
	VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, e.AgentId, e.MessageId, batchAt, received, e.GetLogs().Source, len(prepared))
	if err != nil {
		return 0, err
	}
	if result.RowsAffected() == 0 {
		return 0, nil
	}
	inserted := 0
	// Bound pipeline memory and avoid a round trip per new log.
	for start := 0; start < len(prepared); start += 128 {
		chunk := prepared[start:min(start+128, len(prepared))]
		batch := &pgx.Batch{}
		for _, p := range chunk {
			r := p.Record
			batch.Queue(`INSERT INTO devices(agent_id,device_id,name,source,state_at,last_seen)
			VALUES($1,$2,$2,$3,$4,$4) ON CONFLICT(agent_id,device_id) DO NOTHING`, r.AgentID, r.DeviceID, r.Source, r.ObservedAt)
			batch.Queue(`INSERT INTO log_records(agent_id,device_id,record_scope,record_id,fingerprint,message_id,source,source_id,
			event_at,observed_at,original_timestamp,received_at,time_basis,severity,facility,host,process,process_id,message,raw,truncated,redacted,file_identity,file_offset,search_text,syslog_metadata,observation_metadata,raw_payload_omitted)
			SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28
            WHERE NOT EXISTS(SELECT 1 FROM log_records old JOIN agent_messages om ON om.agent_id=old.agent_id AND om.message_id=old.message_id
             WHERE $3<>'' AND old.agent_id=$1 AND old.device_id=$2 AND old.record_scope='' AND old.record_id=$4 AND old.source=$7 AND old.source_id=$8 AND om.session_id=$3
             AND (old.observation_metadata->'source'->>'provenance'='simulated' OR om.payload->'logs'->'observation'->'source'->>'provenance'='simulated'))
			ON CONFLICT(agent_id,device_id,record_scope,source,source_id,record_id) DO NOTHING RETURNING id`,
				r.AgentID, r.DeviceID, p.Scope, r.RecordID, p.Fingerprint, r.MessageID, r.Source, r.SourceID, r.EventAt, r.ObservedAt, r.OriginalTimestamp, r.ReceivedAt, r.TimeBasis, r.Severity, r.Facility, r.Host, r.Process, r.ProcessID, r.Message, r.Raw, r.Truncated, r.Redacted, r.FileIdentity, r.Offset, p.SearchText, nullableJSON(r.Syslog), nullableJSON(r.Observation), r.RawPayloadOmitted)
		}
		results := tx.SendBatch(ctx, batch)
		ids := make([]int64, len(chunk))
		for i := range chunk {
			if _, err = results.Exec(); err != nil {
				_ = results.Close()
				return 0, err
			}
			err = results.QueryRow().Scan(&ids[i])
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				_ = results.Close()
				return 0, err
			}
		}
		if err = results.Close(); err != nil {
			return 0, err
		}
		for i, p := range chunk {
			r := p.Record
			if ids[i] == 0 {
				var original []byte
				var legacy bool
				if err = tx.QueryRow(ctx, `SELECT l.fingerprint,l.syslog_metadata IS NULL AND l.observation_metadata IS NULL FROM log_records l
WHERE l.agent_id=$1 AND l.device_id=$2 AND l.source=$4 AND l.source_id=$5 AND l.record_id=$6
AND (l.record_scope=$3 OR ($3<>'' AND l.record_scope='' AND EXISTS(SELECT 1 FROM agent_messages om WHERE om.agent_id=l.agent_id AND om.message_id=l.message_id AND om.session_id=$3
AND (l.observation_metadata->'source'->>'provenance'='simulated' OR om.payload->'logs'->'observation'->'source'->>'provenance'='simulated')))) FOR UPDATE OF l`, r.AgentID, r.DeviceID, p.Scope, r.Source, r.SourceID, r.RecordID).Scan(&original, &legacy); err != nil {
					return 0, err
				}
				if !bytes.Equal(original, p.Fingerprint) {
					if !legacy || len(p.LegacyFingerprint) == 0 || !bytes.Equal(original, p.LegacyFingerprint) {
						return 0, ErrLogConflict
					}
					// A stable legacy file record may acquire additive metadata on
					// reread after Collector upgrade. Preserve its original row/time.
					if _, err = tx.Exec(ctx, `UPDATE log_records SET fingerprint=$7,observation_metadata=$8 WHERE agent_id=$1 AND device_id=$2 AND record_scope=$3 AND source=$4 AND source_id=$5 AND record_id=$6`, r.AgentID, r.DeviceID, p.Scope, r.Source, r.SourceID, r.RecordID, p.Fingerprint, nullableJSON(r.Observation)); err != nil {
						return 0, err
					}
				}
				continue
			}
			inserted++
			if a := r.Audit; a != nil {
				_, err = tx.Exec(ctx, `INSERT INTO audit_records(log_id,event_key,event_id,record_type,uid,auid,pid,executable,syscall,success,result,host)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, ids[i], a.EventKey, a.EventID, a.RecordType, a.UID, a.AUID, a.PID, a.Executable, a.Syscall, a.Success, a.Result, a.Host)
				if err != nil {
					return 0, err
				}
			}
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO log_signal_pending(log_id) SELECT l.id FROM log_records l LEFT JOIN audit_records a ON a.log_id=l.id WHERE l.agent_id=$1 AND l.message_id=$2 AND (l.severity BETWEEN 0 AND 3 OR a.success=false OR (l.process IN ('kernel','sshd','sshd-session') AND NOT l.truncated AND octet_length(l.message)<=8192 AND (l.message LIKE 'Out of memory: Killed process %' OR l.message LIKE 'Memory cgroup out of memory: Killed process %' OR l.message LIKE 'Failed password for %'))) AND l.received_at>now()-interval '5 minutes' ON CONFLICT DO NOTHING`, e.AgentId, e.MessageId)
	return inserted, err
}

type BackfillResult struct {
	Processed int `json:"processed_batches"`
	Inserted  int `json:"inserted_records"`
	Failed    int `json:"failed_batches"`
}

// Explicit, bounded backfill: one payload at a time, in independent transactions.
// It does not re-run Agent/session updates or other domain effects.
func (s *Store) BackfillLogs(ctx context.Context, limit int) (BackfillResult, error) {
	out := BackfillResult{}
	if limit < 1 || limit > 1000 {
		return out, errors.New("backfill limit must be 1..1000")
	}
	type key struct{ agent, message string }
	keys := []key{}
	rows, err := s.pool.Query(ctx, `SELECT m.agent_id,m.message_id FROM agent_messages m LEFT JOIN log_batches b USING(agent_id,message_id)
	WHERE m.kind='logs' AND b.message_id IS NULL ORDER BY m.received_at,m.agent_id,m.message_id LIMIT $1`, limit)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var k key
		if err = rows.Scan(&k.agent, &k.message); err != nil {
			rows.Close()
			return out, err
		}
		keys = append(keys, k)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for _, k := range keys {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return out, err
		}
		var raw []byte
		var received time.Time
		err = tx.QueryRow(ctx, `SELECT payload,received_at FROM agent_messages WHERE agent_id=$1 AND message_id=$2 FOR SHARE`, k.agent, k.message).Scan(&raw, &received)
		e := &pb.AgentEnvelope{}
		if err == nil {
			err = protojson.Unmarshal(raw, e)
		}
		n := 0
		if err == nil {
			n, err = projectLogs(ctx, tx, e, received)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		// Also release the transaction after a failed/cancelled COMMIT.
		_ = tx.Rollback(context.Background())
		if err != nil {
			out.Failed++
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			continue
		}
		out.Processed++
		out.Inserted += n
	}
	return out, nil
}
