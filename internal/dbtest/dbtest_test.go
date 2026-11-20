package dbtest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
)

func TestMain(m *testing.M) { Main(m) }

func TestSharedServerStopsWithoutRyuk(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Postgres-backed test in -short mode")
	}
	if path := os.Getenv("BENCHDB_TEST_CONTAINER_ID_FILE"); path != "" {
		defer func() {
			if sharedServer.container != nil {
				assert.NoError(t, os.WriteFile(path, []byte(sharedServer.container.GetContainerID()), 0600))
			} else if t.Skipped() {
				assert.NoError(t, os.WriteFile(path, nil, 0600))
			}
		}()
		NewPool(t)
		return
	}
	path := filepath.Join(t.TempDir(), "container-id")
	t.Cleanup(func() {
		id, err := os.ReadFile(path)
		if err != nil || len(id) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()
		docker, err := testcontainers.NewDockerClientWithOpts(ctx)
		if !assert.NoError(t, err, "connect for container cleanup") {
			return
		}
		defer docker.Close()
		_, err = docker.ContainerRemove(ctx, string(id), client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		if !errdefs.IsNotFound(err) {
			assert.NoError(t, err, "remove leaked container")
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSharedServerStopsWithoutRyuk$")
	cmd.Env = append(os.Environ(), "TESTCONTAINERS_RYUK_DISABLED=true", "BENCHDB_TEST_CONTAINER_ID_FILE="+path)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	id, err := os.ReadFile(path)
	require.NoError(t, err, "child must record its container")
	if len(id) == 0 {
		t.Skip("child skipped because Postgres could not start")
	}
	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	require.NoError(t, err)
	defer docker.Close()
	_, err = docker.ContainerInspect(ctx, string(id), client.ContainerInspectOptions{})
	assert.True(t, errdefs.IsNotFound(err), "shared container should be gone: %v", err)
}

func TestIsolation(t *testing.T) {
	first, ctx := NewPool(t)
	second, _ := NewPool(t)
	assert.Equal(t, first.Config().ConnConfig.Port, second.Config().ConnConfig.Port)
	_, err := first.Exec(ctx, `CREATE TABLE isolation_probe (value integer); INSERT INTO isolation_probe VALUES (1)`)
	require.NoError(t, err)
	conn, err := pgx.Connect(ctx, first.Config().ConnString())
	require.NoError(t, err)
	var value int
	require.NoError(t, conn.QueryRow(ctx, `SELECT value FROM isolation_probe`).Scan(&value))
	assert.Equal(t, 1, value)
	require.NoError(t, conn.Close(ctx))
	var table *string
	require.NoError(t, second.QueryRow(ctx, `SELECT to_regclass('isolation_probe')::text`).Scan(&table))
	assert.Nil(t, table)
	for _, pool := range []*pgxpool.Pool{first, second} {
		require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('benchmark_result')::text`).Scan(&table))
		require.NotNil(t, table)
	}
	empty, _ := NewEmptyPool(t)
	for _, name := range []string{"benchmark_result", "schema_migrations"} {
		require.NoError(t, empty.QueryRow(ctx, `SELECT to_regclass($1)::text`, name).Scan(&table))
		assert.Nil(t, table)
	}
	var childName string
	t.Run("cleanup", func(t *testing.T) {
		child, childCtx := NewPool(t)
		require.NoError(t, child.QueryRow(childCtx, `SELECT current_database()`).Scan(&childName))
	})
	var exists bool
	require.NoError(t, first.QueryRow(ctx, `SELECT EXISTS (SELECT FROM pg_database WHERE datname = $1)`, childName).Scan(&exists))
	assert.False(t, exists)
}
