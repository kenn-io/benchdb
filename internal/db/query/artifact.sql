-- name: ListResultArtifacts :many
SELECT name, kind, media_type, sha256, octet_length(data)::bigint AS size_bytes
FROM result_artifact WHERE result_id = $1 ORDER BY name;

-- name: GetResultArtifact :one
SELECT name, kind, media_type, sha256, data
FROM result_artifact WHERE result_id = $1 AND name = $2;
