-- Trusted device lifetimes are fixed by a verified passkey login. The existing
-- expires_at is the absolute deadline; ordinary and upgraded sessions remain
-- untrusted. Device identity survives token rotation for reliable revocation.
ALTER TABLE admin_sessions ADD COLUMN trusted_device INTEGER NOT NULL DEFAULT 0
    CHECK (trusted_device IN (0, 1));
ALTER TABLE admin_sessions ADD COLUMN device_name TEXT NOT NULL DEFAULT ''
    CHECK (length(CAST(device_name AS BLOB)) <= 320);
ALTER TABLE admin_sessions ADD COLUMN device_id TEXT
    CHECK (device_id IS NULL OR length(device_id) = 36);
UPDATE admin_sessions SET device_id = session_id;
CREATE INDEX admin_sessions_device_idx ON admin_sessions(device_id);

-- NULL preserves an existing, still valid trusted deadline during a sensitive
-- action's reauthentication. An explicit choice is bound before verification.
ALTER TABLE admin_challenges ADD COLUMN trust_device INTEGER
    CHECK (trust_device IS NULL OR trust_device IN (0, 1));
ALTER TABLE admin_challenges ADD COLUMN device_name TEXT NOT NULL DEFAULT ''
    CHECK (length(CAST(device_name AS BLOB)) <= 320);
