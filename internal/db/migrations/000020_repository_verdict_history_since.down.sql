CREATE OR REPLACE FUNCTION invalidate_result_verdicts(result_run text, result_commit text, result_repository text, result_benchmark text, result_deleted boolean)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE is_default boolean;
BEGIN
    SELECT c.sha = c.fork_point_sha INTO is_default FROM commit c WHERE c.id = result_commit FOR SHARE;
    PERFORM enqueue_verdict('run', result_run);
    IF coalesce(is_default, false) THEN
        PERFORM enqueue_verdict('repository', result_repository);
    END IF;
    -- A demoted commit's deferred work cannot discover its last deleted
    -- result. Remove any stored benchmark status even when it is off branch.
    IF coalesce(is_default, false) OR result_deleted THEN
        PERFORM enqueue_verdict('benchmark', result_benchmark);
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION queue_commit_verdicts() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        PERFORM enqueue_verdict('commit', NEW.id);
    END IF;
    IF coalesce(NEW.sha = NEW.fork_point_sha, false)
       OR (TG_OP = 'UPDATE' AND coalesce(OLD.sha = OLD.fork_point_sha, false)) THEN
        PERFORM enqueue_verdict('repository', NEW.repository);
    END IF;
    RETURN NULL;
END;
$$;

DROP FUNCTION enqueue_repository_verdict(text, timestamp without time zone);
DROP FUNCTION bump_repository_generation(text);
DROP TABLE repository_verdict_generation;

CREATE OR REPLACE FUNCTION enqueue_verdict(queue_kind text, queue_key text) RETURNS void LANGUAGE sql AS $$
    INSERT INTO verdict_queue (kind, key) VALUES (queue_kind, queue_key)
    ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
$$;

DROP INDEX run_verdict_baseline_commits_index;
ALTER TABLE run_verdict DROP COLUMN history_until, DROP COLUMN baseline_commit_ids;
ALTER TABLE verdict_queue DROP COLUMN history_since;
