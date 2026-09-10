CREATE TABLE result_artifact (
    result_id varchar(32) NOT NULL REFERENCES benchmark_result(id) ON DELETE CASCADE,
    name text NOT NULL,
    kind text NOT NULL,
    media_type text NOT NULL,
    sha256 text NOT NULL,
    data bytea NOT NULL CHECK (octet_length(data) BETWEEN 1 AND 16777216),
    PRIMARY KEY (result_id, name)
);
