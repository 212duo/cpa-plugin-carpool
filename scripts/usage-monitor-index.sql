-- Run once with write access to the Manager Plus usage database.
-- The plugin itself opens this database read-only and never creates indexes.
-- Re-running this statement is safe. Allow time/disk space for a large table.
CREATE INDEX IF NOT EXISTS idx_carpool_usage_key_time
ON usage_events(api_key_hash, timestamp_ms DESC, id DESC);
