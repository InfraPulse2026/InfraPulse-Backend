-- Widen existing profiles without rewriting the already-applied migration 002.
ALTER TABLE monitoring_policies
    DROP CONSTRAINT monitoring_policies_telemetry_interval_seconds_check,
    DROP CONSTRAINT monitoring_policies_inventory_interval_seconds_check,
    DROP CONSTRAINT monitoring_policies_topology_interval_seconds_check,
    ADD CONSTRAINT monitoring_policies_telemetry_interval_seconds_check
        CHECK (telemetry_interval_seconds BETWEEN 3 AND 86400),
    ADD CONSTRAINT monitoring_policies_inventory_interval_seconds_check
        CHECK (inventory_interval_seconds BETWEEN 3 AND 86400),
    ADD CONSTRAINT monitoring_policies_topology_interval_seconds_check
        CHECK (topology_interval_seconds BETWEEN 3 AND 86400);
