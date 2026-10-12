CREATE OR REPLACE FUNCTION queue_commit_verdicts() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    affected record;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        FOR affected IN
            SELECT DISTINCT br.run_id FROM benchmark_result br WHERE br.commit_id = NEW.id ORDER BY br.run_id
        LOOP
            PERFORM enqueue_verdict('run', affected.run_id);
        END LOOP;
    END IF;
    IF NEW.sha = NEW.fork_point_sha THEN
        PERFORM enqueue_verdict('repository', NEW.repository);
    END IF;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION queue_result_verdicts() RETURNS trigger LANGUAGE plpgsql AS $$
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

DELETE FROM verdict_queue WHERE kind = 'benchmark';
ALTER TABLE verdict_queue DROP CONSTRAINT verdict_queue_kind_check;
ALTER TABLE verdict_queue ADD CONSTRAINT verdict_queue_kind_check CHECK (kind IN ('run', 'repository'));

DROP TABLE IF EXISTS benchmark_verdict;
