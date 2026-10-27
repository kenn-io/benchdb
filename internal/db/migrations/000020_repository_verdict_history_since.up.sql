-- A pull-request run's verdict reads two kinds of default-branch evidence:
-- every ancestry commit its baseline search examined, from the run's
-- recorded base or fork point, and history up to the chosen baseline's commit
-- time. Each verdict records both, so new default-branch results recheck only
-- runs that read them:
-- - a result on an examined commit queues the run directly;
-- - a repository key carries the earliest commit time of the history that
--   queued it, and rechecks runs whose history reaches that time.
-- A NULL baseline_commit_ids means the search depended on commits it could
-- not enumerate, such as an unknown start or an ancestry that stopped early;
-- such runs recheck on any change, as does a NULL history_since.
ALTER TABLE verdict_queue ADD COLUMN history_since timestamp without time zone;
ALTER TABLE run_verdict
  ADD COLUMN baseline_commit_ids text[],
  ADD COLUMN history_until timestamp without time zone;
CREATE INDEX run_verdict_baseline_commits_index ON run_verdict USING gin (baseline_commit_ids);

-- A repository's generation counts changes to its default-branch history.
-- Writers bump it inside their own transaction, so the row stays locked until
-- they commit. A verdict evaluation records the generation before it reads
-- anything and checks it again when it stores the verdict; a change in
-- between, even one not yet committed then, leaves the run queued to be
-- evaluated again.
CREATE TABLE repository_verdict_generation (
    repository text PRIMARY KEY,
    generation bigint NOT NULL
);

CREATE FUNCTION bump_repository_generation(changed_repository text) RETURNS void LANGUAGE sql AS $$
    INSERT INTO repository_verdict_generation (repository, generation) VALUES (changed_repository, 1)
    ON CONFLICT (repository) DO UPDATE SET generation = repository_verdict_generation.generation + 1;
$$;

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
        PERFORM bump_repository_generation(NEW.repository);
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
    -- Queue the result's own run and recent runs whose baseline search
    -- examined this commit, within the same 14 days the worker's repository
    -- recheck covers. Run keys go in run_id order like every multi-key
    -- writer, so concurrent writers cannot deadlock on queue rows.
    INSERT INTO verdict_queue (kind, key)
    SELECT 'run', queued.run_id
    FROM (
        SELECT result_run AS run_id
        UNION
        SELECT v.run_id FROM run_verdict v
        WHERE coalesce(is_default, false) AND v.repository = result_repository
          AND v.last_result_at >= (now() AT TIME ZONE 'UTC') - interval '14 days'
          AND v.baseline_commit_ids @> ARRAY[result_commit]::text[]
    ) queued
    ORDER BY 2
    ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp(), history_since = NULL;
    IF coalesce(is_default, false) THEN
        PERFORM enqueue_repository_verdict(result_repository, commit_at);
        PERFORM bump_repository_generation(result_repository);
    END IF;
    -- A demoted commit's deferred work cannot discover its last deleted
    -- result. Remove any stored benchmark status even when it is off branch.
    IF coalesce(is_default, false) OR result_deleted THEN
        PERFORM enqueue_verdict('benchmark', result_benchmark);
    END IF;
END;
$$;
