-- Archive receipts outlive manifests. The implicit local binding deliberately
-- stores no mount path: another process may mount the same dataset elsewhere.
CREATE TABLE archive_storage (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    dataset_id uuid NOT NULL REFERENCES archive_dataset(id),
    backend text NOT NULL CHECK (backend IN ('file', 's3', 'gs')),
    namespace text NOT NULL,
    endpoint text NOT NULL DEFAULT '',
    active boolean NOT NULL DEFAULT false,
    UNIQUE(dataset_id, backend, namespace, endpoint)
);
CREATE UNIQUE INDEX archive_storage_active ON archive_storage(active) WHERE active;
INSERT INTO archive_storage(dataset_id,backend,namespace,active)
SELECT id,'file','',true FROM archive_dataset;
CREATE FUNCTION check_active_archive_storage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (SELECT count(*) FROM archive_storage WHERE active) <> 1 THEN
        RAISE EXCEPTION 'exactly one archive storage binding must be active';
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER archive_storage_active_check AFTER INSERT OR UPDATE OR DELETE ON archive_storage
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_active_archive_storage();

CREATE TABLE archive_object (
    storage_id uuid NOT NULL REFERENCES archive_storage(id),
    key text NOT NULL,
    byte_size bigint NOT NULL CHECK (byte_size >= 0),
    sha256 bytea CHECK (octet_length(sha256)=32),
    checksums jsonb CHECK (checksums IS NULL OR jsonb_typeof(checksums)='array'),
    revision text,
    verification_method text CHECK (verification_method IN ('provider-sha256','provider-crc32c','readback-sha256')),
    PRIMARY KEY(storage_id,key),
    CHECK ((sha256 IS NULL AND checksums IS NULL AND verification_method IS NULL)
        OR (sha256 IS NOT NULL AND checksums IS NOT NULL AND verification_method IS NOT NULL))
);
-- No filesystem reads, hashing, logical IDs, or per-result rows on upgrade.
INSERT INTO archive_object(storage_id,key,byte_size)
SELECT s.id,f.key,f.bytes FROM archive_storage s CROSS JOIN (
    SELECT path AS key,byte_size AS bytes FROM archive_file UNION
    SELECT runs_path,runs_bytes FROM archive_file UNION
    SELECT series_path,series_bytes FROM archive_file
) f WHERE s.active;

CREATE TABLE archive_cycle (
    id uuid PRIMARY KEY,
    owner text NOT NULL,
    state text NOT NULL CHECK (state IN ('running','committed','abandoned')),
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at timestamptz,
    CHECK ((state='running') = (finished_at IS NULL))
);
CREATE TABLE archive_upload (
    storage_id uuid NOT NULL REFERENCES archive_storage(id),
    key text NOT NULL,
    cycle_id uuid NOT NULL REFERENCES archive_cycle(id),
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expected_size bigint NOT NULL CHECK (expected_size >= 0),
    expected_sha256 bytea NOT NULL CHECK (octet_length(expected_sha256)=32),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','verified','abandoned')),
    receipt jsonb,
    PRIMARY KEY(storage_id,key),
    CHECK (state <> 'verified' OR receipt IS NOT NULL)
);
CREATE TABLE archive_retirement (
    retired_xid xid8 NOT NULL DEFAULT pg_current_xact_id(),
    storage_id uuid NOT NULL REFERENCES archive_storage(id),
    key text NOT NULL,
    first_observed_at timestamptz,
    delete_after timestamptz,
    PRIMARY KEY(storage_id,key),
    CHECK ((first_observed_at IS NULL) = (delete_after IS NULL)),
    CHECK (delete_after >= first_observed_at)
);
CREATE TABLE archive_orphan (
    storage_id uuid NOT NULL REFERENCES archive_storage(id),
    key text NOT NULL,
    revision text,
    first_observed_at timestamptz NOT NULL,
    delete_after timestamptz NOT NULL CHECK (delete_after >= first_observed_at),
    PRIMARY KEY(storage_id,key)
);
CREATE TABLE archive_policy (
    singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
    configured_retention_ns bigint CHECK (configured_retention_ns > 0),
    applied_retention_ns bigint CHECK (applied_retention_ns > 0),
    applied_at timestamptz
);
INSERT INTO archive_policy DEFAULT VALUES;
CREATE TABLE archive_maintenance (
    singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
    status jsonb NOT NULL,
    import_completed boolean NOT NULL CHECK(import_completed)
);

CREATE TABLE archive_file_location (
    id uuid PRIMARY KEY,
    archive_path text REFERENCES archive_file(path)
);
CREATE INDEX archive_file_location_path ON archive_file_location(archive_path);
CREATE INDEX archive_file_commit_commit_id ON archive_file_commit(commit_id);

-- PostgreSQL bytea comparison preserves raw result-ID byte order. NULL bounds
-- are unbounded; half-open ranges allow adjacent partitions to share a boundary.
CREATE TYPE archive_key_range AS RANGE(subtype=bytea);
CREATE TABLE archive_lookup_partition (
    id uuid PRIMARY KEY,
    kind text NOT NULL CHECK(kind IN ('result','run','fingerprint')),
    key_encoding text NOT NULL,
    lower_bound bytea,
    upper_bound bytea,
    key_range archive_key_range GENERATED ALWAYS AS (archive_key_range(lower_bound,upper_bound,'[)')) STORED,
    generation bigint NOT NULL CHECK(generation >= 0),
    complete boolean NOT NULL DEFAULT false,
    backfill_state jsonb NOT NULL DEFAULT '{}',
    CHECK ((kind='result' AND key_encoding='raw-id') OR (kind IN ('run','fingerprint') AND key_encoding='sha256')),
    CHECK (lower_bound IS NULL OR upper_bound IS NULL OR lower_bound < upper_bound),
    EXCLUDE USING gist (key_range WITH &&) WHERE (kind='result'),
    EXCLUDE USING gist (key_range WITH &&) WHERE (kind='run'),
    EXCLUDE USING gist (key_range WITH &&) WHERE (kind='fingerprint')
);
CREATE TABLE archive_lookup_segment (
    partition_id uuid NOT NULL REFERENCES archive_lookup_partition(id),
    storage_id uuid NOT NULL,
    key text NOT NULL,
    row_count bigint NOT NULL CHECK(row_count >= 0),
    byte_size bigint NOT NULL CHECK(byte_size >= 0),
    PRIMARY KEY(partition_id,storage_id,key),
    FOREIGN KEY(storage_id,key) REFERENCES archive_object(storage_id,key)
);
