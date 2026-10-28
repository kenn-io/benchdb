package columnar

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"cloud.google.com/go/auth"
	"github.com/aws/aws-sdk-go-v2/config"
	"go.kenn.io/kit/objectstore"
	"go.kenn.io/kit/objectstore/gcs"
	"go.kenn.io/kit/objectstore/local"
	"go.kenn.io/kit/objectstore/s3"
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

// localArchiveObjects is a local archive. Staging recovery asserts this type,
// so it never runs against a remote backend.
type localArchiveObjects struct {
	archiveObjects
	root string
}

// openArchiveObjects opens the configured archive root through Kit. Local
// keys are the manifest's relative paths under the root. S3 resolves the AWS
// credential chain, including temporary credentials; GCS uses google, the
// OAuth credential DuckDB also uses. Opening makes no remote requests.
func openArchiveObjects(ctx context.Context, location archiveLocation, google *auth.Credentials) (archiveObjects, error) {
	switch location.backend {
	case backendS3:
		aws, err := config.LoadDefaultConfig(ctx, config.WithRegion(location.region))
		if err != nil {
			return nil, fmt.Errorf("load the AWS credential chain for the S3 archive: %w", err)
		}
		store, err := s3.New(ctx, s3.Config{
			Bucket: location.bucket, Prefix: location.prefix, Region: location.region, Endpoint: location.endpoint,
			Credentials: aws.Credentials, ForcePathStyle: location.pathStyle,
			// The operator chose the endpoint's scheme.
			AllowInsecureTransport: strings.HasPrefix(location.endpoint, "http://"),
			// An endpoint without provider checksums is verified by readback.
			AllowReadback: true,
		})
		if err != nil {
			return nil, fmt.Errorf("open S3 archive objects: %w", err)
		}
		return store, nil
	case backendGCS:
		store, err := gcs.New(ctx, gcs.Config{Bucket: location.bucket, Prefix: location.prefix, Credentials: google})
		if err != nil {
			return nil, fmt.Errorf("open GCS archive objects: %w", err)
		}
		return store, nil
	}
	store, err := local.New(location.root)
	if err != nil {
		return nil, fmt.Errorf("open local archive objects: %w", err)
	}
	// Kit publishes under the resolved root, so recovery must walk that same
	// directory: a walk does not descend into a symlinked root.
	root, err := filepath.EvalSymlinks(location.root)
	if err != nil {
		return nil, fmt.Errorf("resolve local archive root: %w", err)
	}
	return &localArchiveObjects{archiveObjects: store, root: root}, nil
}
