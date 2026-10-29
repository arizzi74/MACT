-- Claude runtime maintenance remains independent of worker and Codex outcomes.
-- NULL preserves results completed by older workers.
ALTER TABLE worker_update_requests ADD COLUMN claude_report TEXT
    CHECK (claude_report IS NULL OR (json_valid(claude_report) AND json_type(claude_report)='object'));
