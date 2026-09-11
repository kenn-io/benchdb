CREATE TABLE result_artifact (
    id uuid PRIMARY KEY,
    result_id varchar(50) NOT NULL REFERENCES benchmark_result(id) ON DELETE CASCADE,
    name text NOT NULL,
    kind text NOT NULL,
    media_type text NOT NULL,
    sha256 text NOT NULL,
    size_bytes bigint NOT NULL,
    object_key text NOT NULL UNIQUE
);
CREATE INDEX result_artifact_result_id_idx ON result_artifact(result_id);

-- Deleting metadata must not lose the key needed to remove the stored bytes.
CREATE TABLE artifact_garbage (
    object_key text PRIMARY KEY
);
CREATE FUNCTION queue_artifact_deletion() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO artifact_garbage (object_key) VALUES (OLD.object_key)
    ON CONFLICT DO NOTHING;
    RETURN OLD;
END;
$$;
CREATE TRIGGER result_artifact_delete AFTER DELETE ON result_artifact
FOR EACH ROW EXECUTE FUNCTION queue_artifact_deletion();
