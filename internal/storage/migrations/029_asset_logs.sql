-- Exact Collector/device/simulation-session joins for the additive Asset view.
-- Existing business identities, retained records and legacy APIs are unchanged.
CREATE INDEX log_records_asset_scope_cursor_idx ON log_records(agent_id,device_id,record_scope,id DESC);
CREATE INDEX asset_bindings_log_owner_idx ON asset_source_bindings(source_instance_id,external_id,scope,asset_id)
    WHERE external_type='device' AND legacy_agent_id IS NOT NULL AND legacy_device_id IS NOT NULL;
