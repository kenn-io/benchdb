-- Preserve the large existing heap while new ingestion starts on a small table.
-- The archive worker drains the backlog in bounded batches after deployment.
ALTER TABLE benchmark_result RENAME TO benchmark_result_backlog;
ALTER TABLE benchmark_result_backlog ADD COLUMN arrived_at timestamptz NOT NULL DEFAULT '-infinity';
ALTER TABLE result_artifact DROP CONSTRAINT result_artifact_result_id_fkey;
DROP TRIGGER benchmark_result_verdicts ON benchmark_result_backlog;
DROP TRIGGER benchmark_result_annotation_verdicts ON benchmark_result_backlog;
DROP FUNCTION queue_result_verdicts();
DO $$
DECLARE idx record;
BEGIN
    FOR idx IN SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'benchmark_result_backlog'
    LOOP
        EXECUTE format('ALTER INDEX %I RENAME TO %I', idx.indexname, replace(idx.indexname, 'benchmark_result_', 'br_backlog_'));
    END LOOP;
END;
$$;

CREATE TABLE public.benchmark_result (
    id character varying(50) NOT NULL PRIMARY KEY,
    case_id character varying(50) NOT NULL,
    context_id character varying(50) NOT NULL,
    run_id text NOT NULL,
    unit text,
    time_unit text,
    batch_id text,
    "timestamp" timestamp without time zone NOT NULL,
    iterations integer,
    min numeric,
    max numeric,
    mean numeric,
    median numeric,
    stdev numeric,
    q1 numeric,
    q3 numeric,
    iqr numeric,
    info_id character varying(50) NOT NULL,
    error jsonb,
    data numeric[],
    times numeric[],
    validation jsonb,
    optional_benchmark_info jsonb,
    change_annotations jsonb,
    run_tags jsonb NOT NULL,
    run_reason text,
    commit_id character varying(50),
    hardware_id character varying(50) NOT NULL,
    commit_repo_url text NOT NULL,
    history_fingerprint text NOT NULL,
    arrived_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    submission_key text,
    submission_payload_sha256 text,
    benchmark_id text GENERATED ALWAYS AS (md5((case_id || commit_repo_url))) STORED NOT NULL,
    CONSTRAINT benchmark_result_submission_idempotency_check CHECK (
        ((submission_key IS NULL) AND (submission_payload_sha256 IS NULL))
        OR (
            (submission_key IS NOT NULL)
            AND (submission_payload_sha256 IS NOT NULL)
            AND (submission_payload_sha256 ~ '^[0-9a-f]{64}$'::text)
        )
    )
);

ALTER TABLE benchmark_result ADD FOREIGN KEY (case_id) REFERENCES "case"(id),
    ADD FOREIGN KEY (context_id) REFERENCES context(id),
    ADD FOREIGN KEY (info_id) REFERENCES info(id),
    ADD FOREIGN KEY (hardware_id) REFERENCES hardware(id),
    ADD FOREIGN KEY (commit_id) REFERENCES commit(id);
CREATE UNIQUE INDEX benchmark_result_submission_key_index ON benchmark_result(submission_key) WHERE submission_key IS NOT NULL;
CREATE INDEX benchmark_result_benchmark_id_commit_id_index ON benchmark_result(benchmark_id, commit_id) WHERE error IS NULL;
CREATE INDEX benchmark_result_run_id_index ON benchmark_result(run_id);
CREATE INDEX benchmark_result_timestamp_index ON benchmark_result("timestamp");
CREATE INDEX benchmark_result_arrived_at_index ON benchmark_result(arrived_at, id);
CREATE INDEX benchmark_result_repository_timestamp_run_index ON benchmark_result(commit_repo_url, "timestamp" DESC) INCLUDE(run_id);
CREATE INDEX benchmark_result_history_fingerprint_index ON benchmark_result(history_fingerprint);
CREATE INDEX benchmark_result_commit_id_index ON benchmark_result(commit_id);

CREATE TABLE archive_dataset (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid()
);
INSERT INTO archive_dataset DEFAULT VALUES;
CREATE TABLE archive_file (
    path text PRIMARY KEY,
    partition_month date NOT NULL,
    row_count bigint NOT NULL CHECK (row_count > 0),
    byte_size bigint NOT NULL CHECK (byte_size > 0),
    runs_path text NOT NULL,
    runs_bytes bigint NOT NULL CHECK (runs_bytes > 0),
    series_path text NOT NULL,
    series_bytes bigint NOT NULL CHECK (series_bytes > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX archive_file_month_index ON archive_file(partition_month);
-- Capture the commit inputs used to build each file's series catalog. Include
-- unknown and off-branch commits so later repairs cannot leave stale summaries.
CREATE TABLE archive_file_commit (
    archive_path text NOT NULL REFERENCES archive_file(path) ON DELETE CASCADE,
    commit_id text NOT NULL,
    commit_timestamp timestamp without time zone,
    on_default_branch boolean NOT NULL,
    PRIMARY KEY (archive_path, commit_id)
);
CREATE SEQUENCE result_mutation_version_seq;
CREATE TABLE result_mutation (
    result_id text PRIMARY KEY,
    run_id text NOT NULL,
    history_fingerprint text NOT NULL,
    version bigint NOT NULL DEFAULT nextval('result_mutation_version_seq'),
    partition_month date NOT NULL,
    annotations jsonb,
    deleted boolean NOT NULL DEFAULT false
);
CREATE INDEX result_mutation_month_index ON result_mutation(partition_month);
CREATE TABLE result_submission (
    submission_key text PRIMARY KEY,
    result_id text NOT NULL UNIQUE,
    run_id text NOT NULL,
    history_fingerprint text NOT NULL,
    payload_sha256 text NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$')
);
INSERT INTO result_submission
SELECT submission_key, id, run_id, history_fingerprint, submission_payload_sha256
FROM benchmark_result_backlog WHERE submission_key IS NOT NULL;
CREATE TABLE run_membership (
    run_id text NOT NULL,
    repository text NOT NULL,
    result_count bigint NOT NULL CHECK (result_count >= 0),
    PRIMARY KEY (run_id, repository)
);
INSERT INTO run_membership
SELECT run_id, commit_repo_url, count(*) FROM benchmark_result_backlog GROUP BY run_id, commit_repo_url;
CREATE TABLE benchmark_membership (
    benchmark_id text PRIMARY KEY,
    repository text NOT NULL,
    case_id text NOT NULL,
    result_count bigint NOT NULL CHECK (result_count >= 0),
    nonerror_count bigint NOT NULL CHECK (nonerror_count >= 0)
);
INSERT INTO benchmark_membership
SELECT benchmark_id, commit_repo_url, case_id, count(*), count(*) FILTER (WHERE error IS NULL)
FROM benchmark_result_backlog GROUP BY benchmark_id, commit_repo_url, case_id;

ALTER TABLE benchmark_verdict ADD COLUMN point_count bigint NOT NULL DEFAULT 0;
UPDATE benchmark_verdict SET point_count = cardinality(point_timestamps);
ALTER TABLE benchmark_verdict DROP COLUMN point_timestamps;
ALTER TABLE benchmark_verdict ALTER COLUMN point_count DROP DEFAULT;
ALTER TABLE verdict_queue DROP CONSTRAINT verdict_queue_kind_check;
ALTER TABLE verdict_queue ADD CONSTRAINT verdict_queue_kind_check CHECK (kind IN ('run', 'repository', 'benchmark', 'commit'));

-- Commit repair records one durable expansion key. The analytical worker finds
-- every affected run and benchmark across hot facts and published Parquet.
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

CREATE FUNCTION invalidate_result_verdicts(result_run text, result_commit text, result_repository text, result_benchmark text, result_deleted boolean)
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

CREATE FUNCTION remove_result_metadata(result_id text, result_run text, result_repository text, result_benchmark text, result_error boolean)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    UPDATE run_membership SET result_count = result_count - 1 WHERE run_id = result_run AND repository = result_repository;
    DELETE FROM run_membership WHERE run_id = result_run AND repository = result_repository AND result_count = 0;
    UPDATE benchmark_membership SET result_count = result_count - 1, nonerror_count = nonerror_count - CASE WHEN result_error THEN 0 ELSE 1 END
    WHERE benchmark_id = result_benchmark;
    DELETE FROM benchmark_membership WHERE benchmark_id = result_benchmark AND result_count = 0;
    DELETE FROM result_submission s WHERE s.result_id = remove_result_metadata.result_id;
    DELETE FROM result_artifact a WHERE a.result_id = remove_result_metadata.result_id;
END;
$$;

CREATE FUNCTION maintain_result_metadata() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF current_setting('benchdb.archive_eviction', true) = 'on' THEN
            RETURN NULL;
        END IF;
        IF NOT EXISTS (SELECT 1 FROM result_mutation WHERE result_id = OLD.id AND deleted) THEN
            PERFORM remove_result_metadata(OLD.id, OLD.run_id, OLD.commit_repo_url, OLD.benchmark_id, OLD.error IS NOT NULL);
            PERFORM invalidate_result_verdicts(OLD.run_id, OLD.commit_id, OLD.commit_repo_url, OLD.benchmark_id, true);
        END IF;
    ELSE
        -- Lock the commit before queue keys, matching logical mutation writes.
        PERFORM 1 FROM commit WHERE id = NEW.commit_id FOR SHARE;
        INSERT INTO run_membership VALUES (NEW.run_id, NEW.commit_repo_url, 1)
        ON CONFLICT (run_id, repository) DO UPDATE SET result_count = run_membership.result_count + 1;
        INSERT INTO benchmark_membership VALUES (NEW.benchmark_id, NEW.commit_repo_url, NEW.case_id, 1, CASE WHEN NEW.error IS NULL THEN 1 ELSE 0 END)
        ON CONFLICT (benchmark_id) DO UPDATE SET result_count = benchmark_membership.result_count + 1,
            nonerror_count = benchmark_membership.nonerror_count + EXCLUDED.nonerror_count;
        IF NEW.submission_key IS NOT NULL THEN
            INSERT INTO result_submission VALUES (NEW.submission_key, NEW.id, NEW.run_id, NEW.history_fingerprint, NEW.submission_payload_sha256);
        END IF;
        PERFORM invalidate_result_verdicts(NEW.run_id, NEW.commit_id, NEW.commit_repo_url, NEW.benchmark_id, false);
    END IF;
    RETURN NULL;
END;
$$;
CREATE TRIGGER benchmark_result_metadata AFTER INSERT OR DELETE ON benchmark_result
FOR EACH ROW EXECUTE FUNCTION maintain_result_metadata();
CREATE TRIGGER benchmark_result_backlog_metadata AFTER DELETE ON benchmark_result_backlog
FOR EACH ROW EXECUTE FUNCTION maintain_result_metadata();

CREATE FUNCTION reject_result_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'benchmark facts are immutable; use result_mutation for annotations or deletion';
END;
$$;
CREATE TRIGGER benchmark_result_immutable BEFORE UPDATE ON benchmark_result
FOR EACH ROW EXECUTE FUNCTION reject_result_update();
CREATE TRIGGER benchmark_result_backlog_immutable BEFORE UPDATE ON benchmark_result_backlog
FOR EACH ROW EXECUTE FUNCTION reject_result_update();

CREATE VIEW benchmark_result_local AS
SELECT
    r.id,
    r.case_id,
    r.context_id,
    r.run_id,
    r.unit,
    r.time_unit,
    r.batch_id,
    r."timestamp",
    r.iterations,
    r.min,
    r.max,
    r.mean,
    r.median,
    r.stdev,
    r.q1,
    r.q3,
    r.iqr,
    r.info_id,
    r.error,
    r.data,
    r.times,
    r.validation,
    r.optional_benchmark_info,
    (CASE WHEN m.result_id IS NOT NULL THEN m.annotations ELSE r.change_annotations END)::jsonb AS change_annotations,
    r.run_tags,
    r.run_reason,
    r.commit_id,
    r.hardware_id,
    r.commit_repo_url,
    r.history_fingerprint,
    r.submission_key,
    r.submission_payload_sha256,
    r.benchmark_id
FROM (
    SELECT id, case_id, context_id, run_id, unit, time_unit, batch_id, "timestamp", iterations, min, max, mean, median, stdev, q1, q3, iqr, info_id, error, data, times, validation, optional_benchmark_info, change_annotations, run_tags, run_reason, commit_id, hardware_id, commit_repo_url, history_fingerprint, submission_key, submission_payload_sha256, benchmark_id FROM benchmark_result
    UNION ALL
    SELECT id, case_id, context_id, run_id, unit, time_unit, batch_id, "timestamp", iterations, min, max, mean, median, stdev, q1, q3, iqr, info_id, error, data, times, validation, optional_benchmark_info, change_annotations, run_tags, run_reason, commit_id, hardware_id, commit_repo_url, history_fingerprint, submission_key, submission_payload_sha256, benchmark_id FROM benchmark_result_backlog
) r
LEFT JOIN result_mutation m ON m.result_id = r.id
WHERE NOT coalesce(m.deleted, false);

-- Cast in PostgreSQL so DuckDB never truncates numeric values to DECIMAL(18,3).
CREATE VIEW benchmark_result_numeric AS
SELECT
    id,
    case_id,
    context_id,
    run_id,
    unit,
    time_unit,
    batch_id,
    "timestamp",
    iterations,
    min::double precision AS min,
    max::double precision AS max,
    mean::double precision AS mean,
    median::double precision AS median,
    stdev::double precision AS stdev,
    q1::double precision AS q1,
    q3::double precision AS q3,
    iqr::double precision AS iqr,
    info_id,
    error::text AS error,
    data::double precision[] AS data,
    times::double precision[] AS times,
    validation::text AS validation,
    optional_benchmark_info::text AS optional_benchmark_info,
    change_annotations::text AS change_annotations,
    run_tags::text AS run_tags,
    run_reason,
    commit_id,
    hardware_id,
    commit_repo_url,
    history_fingerprint,
    submission_key,
    submission_payload_sha256,
    benchmark_id,
    arrived_at, 'benchmark_result'::text AS source_table
FROM benchmark_result
UNION ALL
SELECT
    id,
    case_id,
    context_id,
    run_id,
    unit,
    time_unit,
    batch_id,
    "timestamp",
    iterations,
    min::double precision AS min,
    max::double precision AS max,
    mean::double precision AS mean,
    median::double precision AS median,
    stdev::double precision AS stdev,
    q1::double precision AS q1,
    q3::double precision AS q3,
    iqr::double precision AS iqr,
    info_id,
    error::text AS error,
    data::double precision[] AS data,
    times::double precision[] AS times,
    validation::text AS validation,
    optional_benchmark_info::text AS optional_benchmark_info,
    change_annotations::text AS change_annotations,
    run_tags::text AS run_tags,
    run_reason,
    commit_id,
    hardware_id,
    commit_repo_url,
    history_fingerprint,
    submission_key,
    submission_payload_sha256,
    benchmark_id,
    arrived_at, 'benchmark_result_backlog'::text AS source_table
FROM benchmark_result_backlog;
