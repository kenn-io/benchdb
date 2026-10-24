package columnar

import (
	"context"
	"fmt"
	"io"

	"go.kenn.io/kit/objectstore"
	"go.kenn.io/kit/objectstore/local"
)

// archiveObjects is the immutable object API BenchDB consumes from Kit. Keys
// are the manifest's generated relative paths, such as "data/<file>.parquet".
type archiveObjects interface {
	Publish(ctx context.Context, key string, source io.ReadSeeker, size int64, digest [32]byte) (objectstore.Receipt, error)
	Inspect(ctx context.Context, key string) (objectstore.Metadata, error)
	OpenRange(ctx context.Context, key, revision string, offset, length int64) (io.ReadCloser, error)
	ListPage(ctx context.Context, prefix, token string, limit int) (objectstore.Page, error)
	Delete(ctx context.Context, key, revision string) error
}

// openArchiveObjects opens the implicit local archive, whose root is the
// working directory, so existing manifest paths remain valid object keys.
func openArchiveObjects(dir string) (archiveObjects, error) {
	store, err := local.New(dir)
	if err != nil {
		return nil, fmt.Errorf("open local archive objects: %w", err)
	}
	return store, nil
}
