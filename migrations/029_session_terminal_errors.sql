-- Backfill the bounded terminal-error projection once. Normal activity reads
-- use session metadata, never scan the event ledger. This partial index keeps
-- the migration proportional to lifecycle events rather than streamed output.
CREATE INDEX events_session_turn_state_idx
    ON events (session_id,runtime_generation DESC,event_seq DESC)
    WHERE kind IN ('user_message','turn_started','turn_completed','turn_interrupted','turn_failed','command_failed');

WITH inputs AS (
    SELECT event.session_id,event.runtime_generation,event.event_seq,event.received_at,
        COALESCE(json_extract(event.payload,'$.turn_id'),'') AS turn_id,
        row_number() OVER (PARTITION BY event.session_id
            ORDER BY event.runtime_generation DESC,event.event_seq DESC) AS position
    FROM events event JOIN runtimes runtime ON runtime.runtime_id=event.runtime_id
    WHERE event.kind IN ('user_message','turn_started')
        AND event.runtime_generation<=runtime.generation
)
UPDATE sessions SET metadata=json_set(metadata,
    '$.terminal_error_turn',(SELECT turn_id FROM inputs WHERE inputs.session_id=sessions.session_id AND position=1),
    '$.terminal_error_generation',(SELECT runtime_generation FROM inputs WHERE inputs.session_id=sessions.session_id AND position=1),
    '$.terminal_error_input_seq',(SELECT event_seq FROM inputs WHERE inputs.session_id=sessions.session_id AND position=1),
    '$.terminal_error_input_received_at',(SELECT received_at FROM inputs WHERE inputs.session_id=sessions.session_id AND position=1),
    '$.terminal_error_input_command_rowid',COALESCE((SELECT MAX(command.rowid) FROM commands command
        WHERE command.session_id=sessions.session_id AND julianday(command.created_at)<=(
            SELECT julianday(received_at) FROM inputs WHERE inputs.session_id=sessions.session_id AND position=1)),0))
WHERE EXISTS (SELECT 1 FROM inputs WHERE inputs.session_id=sessions.session_id AND position=1);

WITH latest AS (
    SELECT event.session_id,event.kind,event.event_id,event.payload,
        row_number() OVER (PARTITION BY event.session_id
            ORDER BY event.runtime_generation DESC,event.event_seq DESC) AS position
    FROM events event JOIN sessions session ON session.session_id=event.session_id
    JOIN runtimes runtime ON runtime.runtime_id=event.runtime_id
    WHERE event.kind IN ('user_message','turn_started','turn_completed','turn_interrupted','turn_failed','command_failed')
        AND event.runtime_generation<=runtime.generation
        AND (event.kind<>'command_failed' OR (COALESCE(session.active_turn_id,'')=''
            AND EXISTS(SELECT 1 FROM commands command
                WHERE command.command_id=json_extract(event.payload,'$.command_id')
                AND command.session_id=session.session_id AND command.operation='start_turn'
                AND command.rowid>COALESCE(json_extract(session.metadata,'$.terminal_error_input_command_rowid'),0)
                AND NOT EXISTS(SELECT 1 FROM commands newer WHERE newer.session_id=command.session_id
                    AND newer.operation IN ('start_turn','steer') AND newer.rowid>command.rowid))))
        AND (event.kind<>'turn_failed' OR (COALESCE(json_extract(event.payload,'$.will_retry'),0)=0
            AND COALESCE(json_extract(event.payload,'$.willRetry'),0)=0))
        AND (event.kind IN ('user_message','turn_started','command_failed')
            OR json_type(session.metadata,'$.terminal_error_generation') IS NULL
            OR event.runtime_generation>json_extract(session.metadata,'$.terminal_error_generation')
            OR (event.runtime_generation=json_extract(session.metadata,'$.terminal_error_generation')
                AND COALESCE(json_extract(event.payload,'$.turn_id'),'')=json_extract(session.metadata,'$.terminal_error_turn')))
)
UPDATE sessions SET metadata=json_set(metadata,'$.terminal_error',json(COALESCE((
    SELECT CASE WHEN kind IN ('turn_failed','command_failed') THEN json_object(
        'turn_id',COALESCE(json_extract(payload,'$.turn_id'),''),
        'message',CASE WHEN json_type(payload,'$.error.message') IS NOT 'text'
            OR trim(json_extract(payload,'$.error.message'))=''
            THEN 'Turn failed.'
            WHEN length(trim(json_extract(payload,'$.error.message')))>8000
            THEN substr(trim(json_extract(payload,'$.error.message')),1,7999)||'…'
            ELSE trim(json_extract(payload,'$.error.message')) END,
        'code',substr(COALESCE(json_extract(payload,'$.error.code'),''),1,256),
        'revision',event_id) ELSE 'null' END
    FROM latest WHERE latest.session_id=sessions.session_id AND position=1
),'null')));
