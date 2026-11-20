package dbtest

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsolation(t *testing.T) {
	first, ctx := NewPool(t)
	second, _ := NewPool(t)
	assert.Equal(t, first.Config().ConnConfig.Port, second.Config().ConnConfig.Port)
	var firstName, secondName string
	require.NoError(t, first.QueryRow(ctx, `SELECT current_database()`).Scan(&firstName))
	require.NoError(t, second.QueryRow(ctx, `SELECT current_database()`).Scan(&secondName))
	assert.NotEqual(t, firstName, secondName)
	conn, err := pgx.Connect(ctx, first.Config().ConnString())
	require.NoError(t, err)
	var dsnName string
	require.NoError(t, conn.QueryRow(ctx, `SELECT current_database()`).Scan(&dsnName))
	assert.Equal(t, firstName, dsnName)
	require.NoError(t, conn.Close(ctx))
	_, err = first.Exec(ctx, `CREATE TABLE isolation_probe (value integer); INSERT INTO isolation_probe VALUES (1)`)
	require.NoError(t, err)
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
	archive, _, _ := NewArchivePool(t, Postgres15Image, Direct)
	assert.NotEqual(t, first.Config().ConnConfig.Port, archive.Config().ConnConfig.Port)
	var childName string
	t.Run("cleanup", func(t *testing.T) {
		child, childCtx := NewPool(t)
		require.NoError(t, child.QueryRow(childCtx, `SELECT current_database()`).Scan(&childName))
	})
	var exists bool
	require.NoError(t, first.QueryRow(ctx, `SELECT EXISTS (SELECT FROM pg_database WHERE datname = $1)`, childName).Scan(&exists))
	assert.False(t, exists)
}
