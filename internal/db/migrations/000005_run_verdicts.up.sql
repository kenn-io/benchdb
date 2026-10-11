-- Stored CI attention verdicts let the runs list show and filter by attention
-- for every run instead of evaluating a CI report per row on each page load.
-- A run_id can hold results for several repositories; the runs list
-- identifies a run by its latest result within the selected repository, so
-- each repository gets its own verdict. last_result_id breaks timestamp ties
-- the same way the list does.
CREATE TABLE run_verdict (
    run_id text NOT NULL,
    repository text NOT NULL,
    last_result_at timestamp without time zone NOT NULL,
    last_result_id text NOT NULL,
    default_branch boolean NOT NULL,
    needs_attention boolean NOT NULL,
    attention jsonb,
    computed_at timestamptz NOT NULL,
    PRIMARY KEY (run_id, repository)
);
CREATE INDEX run_verdict_attention_index ON run_verdict (last_result_at DESC, run_id DESC)
WHERE needs_attention;

-- Keys whose verdicts must be recomputed. A 'repository' key stands for every
-- recent pull-request run in that repository, because new default-branch
-- history can change their baselines. Writers bump enqueued_at without
-- disturbing a claim; the claiming worker sees the newer version when it
-- completes and releases the key for another pass. claim_token fences writes
-- from a worker whose lease has expired.
CREATE TABLE verdict_queue (
    kind text NOT NULL CHECK (kind IN ('run', 'repository')),
    key text NOT NULL,
    enqueued_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    claimed_until timestamptz,
    claim_token uuid,
    PRIMARY KEY (kind, key)
);
CREATE INDEX verdict_queue_ready_index ON verdict_queue (enqueued_at);

CREATE FUNCTION enqueue_verdict(queue_kind text, queue_key text) RETURNS void LANGUAGE sql AS $$
    INSERT INTO verdict_queue (kind, key) VALUES (queue_kind, queue_key)
    ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
$$;

-- Pages queue the runs they list that have no verdict yet. Read-only
-- connections, such as the production-clone harness, list runs without
-- queuing, because any write there is an error.
CREATE FUNCTION enqueue_missing_run_verdicts(run_ids text[]) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    IF current_setting('transaction_read_only') = 'on' THEN
        RETURN;
    END IF;
    INSERT INTO verdict_queue (kind, key)
    SELECT 'run', ids.run_id
    FROM unnest(run_ids) AS ids(run_id)
    WHERE NOT EXISTS (SELECT 1 FROM run_verdict v WHERE v.run_id = ids.run_id)
    ON CONFLICT DO NOTHING;
END;
$$;

CREATE FUNCTION queue_result_verdicts() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    changed benchmark_result;
BEGIN
    IF TG_OP = 'DELETE' THEN
        changed := OLD;
    ELSE
        changed := NEW;
    END IF;
    PERFORM enqueue_verdict('run', changed.run_id);
    IF EXISTS (
        SELECT 1 FROM commit c
        WHERE c.id = changed.commit_id AND c.sha = c.fork_point_sha
    ) THEN
        PERFORM enqueue_verdict('repository', changed.commit_repo_url);
    END IF;
    RETURN NULL;
END;
$$;
CREATE TRIGGER benchmark_result_verdicts AFTER INSERT OR DELETE ON benchmark_result
FOR EACH ROW EXECUTE FUNCTION queue_result_verdicts();
CREATE TRIGGER benchmark_result_annotation_verdicts AFTER UPDATE OF change_annotations ON benchmark_result
FOR EACH ROW EXECUTE FUNCTION queue_result_verdicts();

-- A new default-branch commit can extend a pull request's ancestry, and a
-- repaired commit can gain the fork point its runs needed.
CREATE FUNCTION queue_commit_verdicts() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.sha = NEW.fork_point_sha THEN
        PERFORM enqueue_verdict('repository', NEW.repository);
    END IF;
    IF TG_OP = 'UPDATE' THEN
        PERFORM enqueue_verdict('run', runs.run_id)
        FROM (SELECT DISTINCT br.run_id FROM benchmark_result br WHERE br.commit_id = NEW.id) runs;
    END IF;
    RETURN NULL;
END;
$$;
CREATE TRIGGER commit_verdicts AFTER INSERT OR UPDATE ON commit
FOR EACH ROW EXECUTE FUNCTION queue_commit_verdicts();

-- Existing deployments start with verdicts for the runs people are likely to
-- open; older runs are queued when a page first shows them.
INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'run', br.run_id
FROM benchmark_result br
WHERE br."timestamp" >= (now() AT TIME ZONE 'UTC') - interval '14 days'
ON CONFLICT DO NOTHING;
