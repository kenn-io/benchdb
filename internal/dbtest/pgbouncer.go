package dbtest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"text/template"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	pgBouncerPort  = "6432/tcp"
	pgBouncerRepo  = "benchdb-test-pgbouncer"
	pgBouncerTag   = "1.26.1"
	pgBouncerDir   = "/etc/pgbouncer"
	dockerfileName = "Dockerfile.pgbouncer"
)

//go:embed testdata/Dockerfile.pgbouncer
var pgBouncerDockerfile []byte

//go:embed testdata/pgbouncer.ini
var pgBouncerConfigTemplate string

// pgBouncerSettings are the per-test values rendered into pgbouncer.ini.
type pgBouncerSettings struct {
	PoolMode     string
	DatabaseHost string
	DatabaseName string
}

// renderPgBouncerConfig renders the per-test pgbouncer.ini.
func renderPgBouncerConfig(settings pgBouncerSettings) ([]byte, error) {
	tmpl, err := template.New("pgbouncer.ini").Option("missingkey=error").Parse(pgBouncerConfigTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse pgbouncer.ini template: %w", err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, settings); err != nil {
		return nil, fmt.Errorf("render pgbouncer.ini: %w", err)
	}
	return out.Bytes(), nil
}

// pgBouncerBuildContext packs the embedded Dockerfile into a build context so
// the fixture builds from any package's test working directory.
func pgBouncerBuildContext() (*bytes.Reader, error) {
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	header := &tar.Header{Name: dockerfileName, Mode: 0o644, Size: int64(len(pgBouncerDockerfile))}
	if err := writer.WriteHeader(header); err != nil {
		return nil, fmt.Errorf("write build context header: %w", err)
	}
	if _, err := writer.Write(pgBouncerDockerfile); err != nil {
		return nil, fmt.Errorf("write build context: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close build context: %w", err)
	}
	return bytes.NewReader(buf.Bytes()), nil
}

// startPgBouncer builds and starts the PgBouncer fixture on the database's
// network, registers its cleanup, and returns a URL that reaches PostgreSQL
// through it. PgBouncer connects to the database by network alias; the host
// only ever sees PgBouncer's mapped port.
func startPgBouncer(
	t *testing.T, ctx context.Context, archiveNet *testcontainers.DockerNetwork,
	mode PoolMode, password string,
) string {
	t.Helper()
	config, err := renderPgBouncerConfig(pgBouncerSettings{
		PoolMode:     mode.String(),
		DatabaseHost: archiveDBAlias,
		DatabaseName: archiveDatabase,
	})
	require.NoError(t, err)
	userlist := fmt.Sprintf("%q %q\n", archiveUser, password)

	var buildLog bytes.Buffer
	container, err := retryReaperRace(ctx, t, "start PgBouncer",
		func() (testcontainers.Container, error) {
			// Each attempt needs fresh readers; a failed attempt consumed the last ones.
			buildContext, err := pgBouncerBuildContext()
			if err != nil {
				return nil, err
			}
			started, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
				FromDockerfile: testcontainers.FromDockerfile{
					ContextArchive: buildContext,
					Dockerfile:     dockerfileName,
					Repo:           pgBouncerRepo,
					Tag:            pgBouncerTag,
					KeepImage:      true,
					BuildLogWriter: &buildLog,
				},
				ExposedPorts:   []string{pgBouncerPort},
				Networks:       []string{archiveNet.Name},
				NetworkAliases: map[string][]string{archiveNet.Name: {"pgbouncer"}},
				Files: []testcontainers.ContainerFile{
					{
						Reader:            bytes.NewReader(config),
						ContainerFilePath: pgBouncerDir + "/pgbouncer.ini",
						FileMode:          0o644,
					},
					{
						Reader:            strings.NewReader(userlist),
						ContainerFilePath: pgBouncerDir + "/userlist.txt",
						FileMode:          0o644,
					},
				},
				WaitingFor: wait.ForListeningPort(pgBouncerPort),
				Started:    true,
			})
			if err != nil {
				_ = testcontainers.TerminateContainer(started)
			}
			return started, err
		})
	testcontainers.CleanupContainer(t, container)
	require.NoError(t, err, "start PgBouncer container (Docker required); build log:\n%s", buildLog.String())

	endpoint, err := container.PortEndpoint(ctx, pgBouncerPort, "")
	require.NoError(t, err, "PgBouncer endpoint")
	url := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", archiveUser, password, endpoint, archiveDatabase)
	requireLogin(t, ctx, url)
	return url
}

// requireLogin proves the URL authenticates through PgBouncer to PostgreSQL.
func requireLogin(t *testing.T, ctx context.Context, url string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	require.NoError(t, err, "connect through PgBouncer")
	defer func() { _ = conn.Close(ctx) }()
	var one int
	require.NoError(t, conn.QueryRow(ctx, `SELECT 1`).Scan(&one), "query through PgBouncer")
}

// randomPassword returns a per-test credential so no fixed secret is shared.
func randomPassword(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 16)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	return hex.EncodeToString(raw)
}
