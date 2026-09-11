-- name: ListResultArtifacts :many
SELECT id, name, kind, media_type, sha256, size_bytes
FROM result_artifact WHERE result_id = $1 ORDER BY name, id;

-- name: GetResultArtifact :one
SELECT * FROM result_artifact WHERE result_id = $1 AND id = $2;

-- name: InsertResultArtifact :exec
INSERT INTO result_artifact (id, result_id, name, kind, media_type, sha256, size_bytes, object_key)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: DeleteResultArtifact :one
DELETE FROM result_artifact WHERE result_id = $1 AND id = $2 RETURNING object_key;

-- name: ListArtifactGarbage :many
SELECT object_key FROM artifact_garbage ORDER BY object_key;

-- name: QueueArtifactGarbage :exec
INSERT INTO artifact_garbage (object_key) VALUES ($1) ON CONFLICT DO NOTHING;

-- name: RemoveArtifactGarbage :exec
DELETE FROM artifact_garbage WHERE object_key = $1;
