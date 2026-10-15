-- Once Parquet has been published, restoring the recovery backup is the
-- rollback path: a SQL migration cannot read files outside PostgreSQL.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM archive_file) THEN
        RAISE EXCEPTION 'restore the pre-migration recovery backup to roll back an archived dataset';
    END IF;
END; $$;
DROP VIEW benchmark_result_numeric;
DROP VIEW benchmark_result_local;
DROP TRIGGER benchmark_result_metadata ON benchmark_result;
DROP TRIGGER benchmark_result_backlog_metadata ON benchmark_result_backlog;
DROP TRIGGER benchmark_result_immutable ON benchmark_result;
DROP TRIGGER benchmark_result_backlog_immutable ON benchmark_result_backlog;
DROP FUNCTION maintain_result_metadata();
DROP FUNCTION reject_result_update();
DROP FUNCTION remove_result_metadata(text, text, text, text, boolean);
DROP FUNCTION invalidate_result_verdicts(text, text, text, text, boolean);
INSERT INTO benchmark_result_backlog (id, case_id, context_id, run_id, unit, time_unit, batch_id, "timestamp", iterations, min, max, mean, median, stdev, q1, q3, iqr, info_id, error, data, times, validation, optional_benchmark_info, change_annotations, run_tags, run_reason, commit_id, hardware_id, commit_repo_url, history_fingerprint, submission_key, submission_payload_sha256) SELECT id, case_id, context_id, run_id, unit, time_unit, batch_id, "timestamp", iterations, min, max, mean, median, stdev, q1, q3, iqr, info_id, error, data, times, validation, optional_benchmark_info, change_annotations, run_tags, run_reason, commit_id, hardware_id, commit_repo_url, history_fingerprint, submission_key, submission_payload_sha256 FROM benchmark_result;
DELETE FROM benchmark_result_backlog br USING result_mutation m WHERE m.result_id = br.id AND m.deleted;
UPDATE benchmark_result_backlog br SET change_annotations = m.annotations FROM result_mutation m WHERE m.result_id = br.id;
DROP TABLE benchmark_result;
ALTER TABLE benchmark_result_backlog RENAME TO benchmark_result;
ALTER TABLE benchmark_result DROP COLUMN arrived_at;
DO $$
DECLARE idx record;
BEGIN
    FOR idx IN SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'benchmark_result'
    LOOP
        EXECUTE format('ALTER INDEX %I RENAME TO %I', idx.indexname, replace(idx.indexname, 'br_backlog_', 'benchmark_result_'));
    END LOOP;
END;
$$;
ALTER TABLE result_artifact ADD CONSTRAINT result_artifact_result_id_fkey FOREIGN KEY (result_id) REFERENCES benchmark_result(id) ON DELETE CASCADE;
DROP TABLE result_mutation, result_submission, run_membership, benchmark_membership, archive_file_commit, archive_file, archive_dataset;
DROP SEQUENCE result_mutation_version_seq;
ALTER TABLE benchmark_verdict DROP COLUMN point_count, ADD COLUMN point_timestamps timestamp without time zone[] NOT NULL DEFAULT '{}';
ALTER TABLE benchmark_verdict ALTER COLUMN point_timestamps DROP DEFAULT;
INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'run', br.run_id FROM benchmark_result br
JOIN verdict_queue q ON q.kind = 'commit' AND q.key = br.commit_id
UNION
SELECT DISTINCT 'benchmark', br.benchmark_id FROM benchmark_result br
JOIN verdict_queue q ON q.kind = 'commit' AND q.key = br.commit_id
ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
DELETE FROM verdict_queue WHERE kind = 'commit';
ALTER TABLE verdict_queue DROP CONSTRAINT verdict_queue_kind_check;
ALTER TABLE verdict_queue ADD CONSTRAINT verdict_queue_kind_check CHECK (kind IN ('run', 'repository', 'benchmark'));
CREATE OR REPLACE FUNCTION queue_result_verdicts() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    changed benchmark_result;
    default_branch boolean;
BEGIN
    IF TG_OP = 'DELETE' THEN
        changed := OLD;
    ELSE
        changed := NEW;
    END IF;
    SELECT c.sha = c.fork_point_sha INTO default_branch
    FROM commit c
    WHERE c.id = changed.commit_id
    FOR SHARE;
    PERFORM enqueue_verdict('run', changed.run_id);
    IF coalesce(default_branch, false) THEN
        PERFORM enqueue_verdict('repository', changed.commit_repo_url);
        PERFORM enqueue_verdict('benchmark', changed.benchmark_id);
    END IF;
    RETURN NULL;
END;
$$;

-- A commit that joins or leaves the default branch, or gains a timestamp,
-- changes the history of every benchmark with results on it.
CREATE OR REPLACE FUNCTION queue_commit_verdicts() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    affected record;
    default_branch boolean := coalesce(NEW.sha = NEW.fork_point_sha, false);
BEGIN
    IF TG_OP = 'UPDATE' THEN
        FOR affected IN
            SELECT DISTINCT br.run_id FROM benchmark_result br WHERE br.commit_id = NEW.id ORDER BY br.run_id
        LOOP
            PERFORM enqueue_verdict('run', affected.run_id);
        END LOOP;
        default_branch := default_branch OR coalesce(OLD.sha = OLD.fork_point_sha, false);
    END IF;
    IF default_branch THEN
        PERFORM enqueue_verdict('repository', NEW.repository);
    END IF;
    IF TG_OP = 'UPDATE' AND default_branch THEN
        FOR affected IN
            SELECT DISTINCT br.benchmark_id FROM benchmark_result br WHERE br.commit_id = NEW.id ORDER BY br.benchmark_id
        LOOP
            PERFORM enqueue_verdict('benchmark', affected.benchmark_id);
        END LOOP;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER benchmark_result_verdicts AFTER INSERT OR DELETE ON benchmark_result
FOR EACH ROW EXECUTE FUNCTION queue_result_verdicts();
CREATE TRIGGER benchmark_result_annotation_verdicts AFTER UPDATE OF change_annotations ON benchmark_result
FOR EACH ROW EXECUTE FUNCTION queue_result_verdicts();
INSERT INTO verdict_queue (kind, key) SELECT DISTINCT 'benchmark', benchmark_id FROM benchmark_verdict
ON CONFLICT (kind, key) DO UPDATE SET enqueued_at = clock_timestamp();
