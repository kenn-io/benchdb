-- Stored benchmark statuses let the Benchmarks list filter, count and order by
-- status across every benchmark instead of scoring recent history on each page
-- load. Each row is one machine's track of a logical benchmark (case and
-- repository) over default-branch history: a machine filter shows that
-- machine's own status, and the unfiltered list takes the worst status across
-- machines. unit is NULL when the track has no single unit with a known
-- direction, which makes the track's status insufficient.
CREATE TABLE benchmark_verdict (
    benchmark_id text NOT NULL,
    machine_name text NOT NULL,
    repository text NOT NULL,
    case_id character varying(50) NOT NULL,
    history_fingerprints text[] NOT NULL,
    unit text,
    status text NOT NULL CHECK (status IN ('regressed', 'improved', 'stable', 'insufficient')),
    latest_result_id text NOT NULL,
    latest_result_timestamp timestamp without time zone NOT NULL,
    latest_commit_sha text NOT NULL,
    latest_commit_timestamp timestamp without time zone NOT NULL,
    computed_at timestamptz NOT NULL,
    PRIMARY KEY (benchmark_id, machine_name)
);

-- A 'benchmark' key recomputes one benchmark_id's machine tracks.
ALTER TABLE verdict_queue DROP CONSTRAINT verdict_queue_kind_check;
ALTER TABLE verdict_queue ADD CONSTRAINT verdict_queue_kind_check
    CHECK (kind IN ('run', 'repository', 'benchmark'));

-- Queue rows are locked in one order everywhere: run keys, then the
-- repository key, then benchmark keys. A transaction locks a benchmark key
-- only after the repository key, so the repository key serializes writers that
-- touch the same benchmarks.
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

-- Existing deployments compute every benchmark with default-branch history.
INSERT INTO verdict_queue (kind, key)
SELECT DISTINCT 'benchmark', br.benchmark_id
FROM benchmark_result br
JOIN commit c ON c.id = br.commit_id
WHERE c.sha = c.fork_point_sha
ORDER BY 2
ON CONFLICT DO NOTHING;
