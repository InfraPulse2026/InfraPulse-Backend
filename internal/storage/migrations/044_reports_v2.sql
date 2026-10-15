ALTER TABLE report_snapshots ADD COLUMN kind text NOT NULL DEFAULT 'legacy' CHECK(kind IN ('legacy','unified'));
CREATE INDEX report_snapshots_kind_created ON report_snapshots(kind,created_at DESC,id);
CREATE INDEX ai_previews_correlated_case ON ai_previews((preview->>'correlated_incident_id'));
