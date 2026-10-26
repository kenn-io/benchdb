-- New default-branch history can change a pull-request run's verdict only if
-- the run forked at or after that history: a later commit is neither in its
-- baseline search nor in its lookback window. A repository key records the
-- earliest commit time among the history that queued it, so the worker
-- re-evaluates only runs that could change. NULL re-evaluates every recent
-- run, as before.
ALTER TABLE verdict_queue ADD COLUMN history_since timestamp without time zone;

-- Any other enqueue of a repository key, such as commit repair, asks for a
-- full re-evaluation.
CREATE OR REPLACE FUNCTION enqueue_verdict(queue_kind text, queue_key text) RETURNS void LANGUAGE sql AS $$
    INSERT INTO verdict_queue (kind, key) VALUES (queue_kind, queue_key)
    ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp(), history_since = NULL;
$$;

CREATE FUNCTION enqueue_repository_verdict(queue_repository text, since timestamp without time zone)
RETURNS void LANGUAGE sql AS $$
    INSERT INTO verdict_queue (kind, key, history_since) VALUES ('repository', queue_repository, since)
    ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp(),
        history_since = CASE
            WHEN verdict_queue.history_since IS NULL OR EXCLUDED.history_since IS NULL THEN NULL
            ELSE least(verdict_queue.history_since, EXCLUDED.history_since)
        END;
$$;

CREATE OR REPLACE FUNCTION queue_commit_verdicts() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        PERFORM enqueue_verdict('commit', NEW.id);
    END IF;
    IF coalesce(NEW.sha = NEW.fork_point_sha, false)
       OR (TG_OP = 'UPDATE' AND coalesce(OLD.sha = OLD.fork_point_sha, false)) THEN
        -- A changed commit's old time matters too: it may have moved.
        IF TG_OP = 'UPDATE' THEN
            PERFORM enqueue_repository_verdict(NEW.repository,
                CASE WHEN OLD."timestamp" IS NULL OR NEW."timestamp" IS NULL THEN NULL
                     ELSE least(OLD."timestamp", NEW."timestamp") END);
        ELSE
            PERFORM enqueue_repository_verdict(NEW.repository, NEW."timestamp");
        END IF;
    END IF;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION invalidate_result_verdicts(result_run text, result_commit text, result_repository text, result_benchmark text, result_deleted boolean)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    is_default boolean;
    commit_at timestamp without time zone;
BEGIN
    SELECT c.sha = c.fork_point_sha, c."timestamp" INTO is_default, commit_at FROM commit c WHERE c.id = result_commit FOR SHARE;
    PERFORM enqueue_verdict('run', result_run);
    IF coalesce(is_default, false) THEN
        PERFORM enqueue_repository_verdict(result_repository, commit_at);
    END IF;
    -- A demoted commit's deferred work cannot discover its last deleted
    -- result. Remove any stored benchmark status even when it is off branch.
    IF coalesce(is_default, false) OR result_deleted THEN
        PERFORM enqueue_verdict('benchmark', result_benchmark);
    END IF;
END;
$$;
