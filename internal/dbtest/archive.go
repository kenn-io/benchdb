package dbtest

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/network"

	"go.kenn.io/benchdb/internal/db"
)

const (
	// ArchivePostgresImageEnv selects the PostgreSQL image for archive
	// coordination tests. CI sets it from the version matrix.
	ArchivePostgresImageEnv = "BENCHDB_TEST_ARCHIVE_POSTGRES_IMAGE"

	// Postgres15Image is the default image, the same one NewPool uses.
	Postgres15Image = "postgres:15.2-alpine"

	// Postgres17Image is the supported PostgreSQL 17 release. 17.10 was
	// published on 2026-07-08; the index digest was read from Docker Hub on
	// 2026-10-23. Update the tag and digest together.
	Postgres17Image = "postgres:17.10-alpine@sha256:742f40ea20b9ff2ff31db5458d127452988a2164df9e17441e191f3b72252193"

	archiveDatabase = "benchdb"
	archiveUser     = "postgres"
	archiveDBAlias  = "benchdb-postgres"
)

// PoolMode selects how an archive test client reaches PostgreSQL.
type PoolMode int

const (
	// Direct connects to PostgreSQL with no pooler.
	Direct PoolMode = iota
	// Session connects through PgBouncer in session mode, where a client keeps
	// one backend until it disconnects. Archive coordination supports this mode.
	Session
	// Transaction connects through PgBouncer in transaction mode, where a
	// backend can serve a different client after every transaction. Archive
	// coordination does not support this mode.
	Transaction
)

// String names the mode for subtests and failure messages.
func (m PoolMode) String() string {
	switch m {
	case Direct:
		return "direct"
	case Session:
		return "session"
	case Transaction:
		return "transaction"
	default:
		return fmt.Sprintf("PoolMode(%d)", int(m))
	}
}

// ArchivePostgresImage returns the image archive tests run against: the value
// of BENCHDB_TEST_ARCHIVE_POSTGRES_IMAGE, or PostgreSQL 15.2 when unset.
func ArchivePostgresImage() string {
	if image := os.Getenv(ArchivePostgresImageEnv); image != "" {
		return image
	}
	return Postgres15Image
}

// NewArchivePool starts PostgreSQL from postgresImage, applies the schema, and
// returns a direct pool, the connection URL for mode, and a context. For
// Session and Transaction it also starts PgBouncer on a private network that
// reaches PostgreSQL by alias, and the returned URL points at PgBouncer.
//
// Unlike NewPool it never skips because Docker or a container is unavailable;
// only an explicit `go test -short` skips. The image tag must match the
// server's measured server_version_num so a mislabelled matrix entry fails
// instead of testing the wrong release.
func NewArchivePool(
	t *testing.T, postgresImage string, mode PoolMode,
) (*pgxpool.Pool, string, context.Context) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping archive coordination test in -short mode")
	}
	ctx := context.Background()

	archiveNet, err := network.New(ctx)
	require.NoError(t, err, "create archive test network")
	testcontainers.CleanupNetwork(t, archiveNet)

	password := randomPassword(t)
	container, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase(archiveDatabase),
		postgres.WithUsername(archiveUser),
		postgres.WithPassword(password),
		postgres.BasicWaitStrategies(),
		network.WithNetwork([]string{archiveDBAlias}, archiveNet),
	)
	testcontainers.CleanupContainer(t, container)
	require.NoError(t, err, "start Postgres container %s (Docker required)", postgresImage)

	directURL, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err, "connection string")
	pool, err := pgxpool.New(ctx, directURL)
	require.NoError(t, err, "open pool")
	t.Cleanup(pool.Close)

	var serverVersionNum int
	require.NoError(t, pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersionNum))
	require.NoError(t, checkServerVersion(postgresImage, serverVersionNum))
	require.NoError(t, db.Migrate(ctx, pool), "apply schema")

	switch mode {
	case Direct:
		return pool, directURL, ctx
	case Session, Transaction:
		url := startPgBouncer(t, ctx, archiveNet, mode, password)
		return pool, url, ctx
	default:
		require.Failf(t, "unknown pool mode", "%v", mode)
		return nil, "", ctx
	}
}

var imageVersionPattern = regexp.MustCompile(`^(\d+)\.(\d+)`)

// expectedServerVersionNum derives PostgreSQL's server_version_num from an
// image reference whose tag starts with major.minor, such as
// "postgres:17.10-alpine@sha256:...".
func expectedServerVersionNum(image string) (int, error) {
	ref, _, _ := strings.Cut(image, "@")
	tag := ref[strings.LastIndex(ref, ":")+1:]
	if !strings.Contains(ref, ":") || strings.Contains(tag, "/") {
		return 0, fmt.Errorf("image %q has no tag; use major.minor such as postgres:17.10-alpine", image)
	}
	match := imageVersionPattern.FindStringSubmatch(tag)
	if match == nil {
		return 0, fmt.Errorf("image %q tag %q does not start with major.minor", image, tag)
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	return major*10000 + minor, nil
}

// checkServerVersion fails when the server a container reports differs from
// the version its image tag claims.
func checkServerVersion(image string, serverVersionNum int) error {
	want, err := expectedServerVersionNum(image)
	if err != nil {
		return err
	}
	if serverVersionNum != want {
		return fmt.Errorf("image %q implies server_version_num %d but the server reports %d",
			image, want, serverVersionNum)
	}
	return nil
}
