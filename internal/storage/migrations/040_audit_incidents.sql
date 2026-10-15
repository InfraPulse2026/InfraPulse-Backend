-- Bounded recent failed audit events; original log and audit rows remain immutable.
INSERT INTO log_signal_pending(log_id)
SELECT l.id FROM log_records l JOIN audit_records a ON a.log_id=l.id
WHERE a.success=false AND l.received_at>now()-interval '5 minutes'
AND l.event_at>now()-interval '5 minutes'
AND NOT EXISTS(SELECT 1 FROM log_incident_records e WHERE e.log_id=l.id)
ORDER BY l.id DESC LIMIT 10000 ON CONFLICT DO NOTHING;
