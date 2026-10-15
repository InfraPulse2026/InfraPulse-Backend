ALTER TABLE asset_source_bindings ADD COLUMN provider_state jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(provider_state)='object');
ALTER TABLE asset_fact_versions ADD COLUMN provider_state jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(provider_state)='object');
