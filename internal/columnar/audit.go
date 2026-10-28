package columnar

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/kit/objectstore"
)

// AuditResult reports one audit of the active storage. Objects and Bytes
// cover every object the manifest and lookup directory reference. Missing
// keys have no object; Changed keys have an object whose size or revision
// differs from its receipt.
type AuditResult struct {
	Objects int
	Bytes   int64
	Missing []string
	Changed []string
}

// errArchiveAuditFailed reports referenced objects that are missing or changed.
var errArchiveAuditFailed = errors.New("archive audit failed")

// auditListLimit bounds the keys an audit error names.
const auditListLimit = 5

// AuditArchive inspects every object that the manifest and lookup directory
// reference in the active storage. Unlike startup, which checks only the
// dataset marker for a remote archive, it requests each object's metadata. A
// reader guard keeps cleanup from removing objects while it runs. It fails
// when any object is missing or differs from its receipt.
func (e *Engine) AuditArchive(ctx context.Context) (result AuditResult, err error) {
	guard, err := e.coord.reader(ctx)
	if err != nil {
		return AuditResult{}, err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
		defer cancel()
		err = errors.Join(err, guard.Close(closeCtx))
	}()
	expected, err := e.auditedObjects(ctx, guard.conn)
	if err != nil {
		return AuditResult{}, err
	}
	for _, object := range expected {
		result.Objects++
		result.Bytes += object.size
		inspectCtx, cancel := context.WithTimeout(ctx, remoteCheckTimeout)
		meta, err := e.objects.Inspect(inspectCtx, object.key)
		cancel()
		switch {
		case errors.Is(err, objectstore.ErrNotFound):
			result.Missing = append(result.Missing, object.key)
		case err != nil:
			return result, fmt.Errorf("inspect archive object %s: %w", object.key, err)
		case meta.Size != object.size || (object.revision != "" && meta.Revision != object.revision):
			result.Changed = append(result.Changed, object.key)
		}
	}
	if len(result.Missing) > 0 || len(result.Changed) > 0 {
		return result, fmt.Errorf("%w: %d of %d referenced objects are missing and %d changed; first: %s",
			errArchiveAuditFailed, len(result.Missing), result.Objects, len(result.Changed),
			strings.Join(firstKeys(append(append([]string(nil), result.Missing...), result.Changed...)), ", "))
	}
	return result, nil
}

type auditedObject struct {
	key      string
	size     int64
	revision string
}

func (e *Engine) auditedObjects(ctx context.Context, q rowsQueryer) ([]auditedObject, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	rows, err := q.Query(ctx, `WITH `+referencedKeys+`
SELECT r.key,o.byte_size,coalesce(o.revision,'') FROM referenced r
LEFT JOIN archive_object o ON o.storage_id=$1 AND o.key=r.key ORDER BY r.key`, e.archive.storageID)
	if err != nil {
		return nil, fmt.Errorf("read the referenced archive objects: %w", err)
	}
	objects, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (auditedObject, error) {
		var object auditedObject
		var size *int64
		if err := row.Scan(&object.key, &size, &object.revision); err != nil {
			return object, err
		}
		if size == nil {
			return object, fmt.Errorf("archive object %s has no receipt in the active storage", object.key)
		}
		object.size = *size
		return object, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the referenced archive objects: %w", err)
	}
	return objects, nil
}

func firstKeys(keys []string) []string {
	return keys[:min(len(keys), auditListLimit)]
}
