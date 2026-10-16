package db_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
)

func TestArchiveStateUpgrade(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "fresh"
		if legacy {
			name = "migration13"
		}
		t.Run(name, func(t *testing.T) {
			pool, ctx := dbtest.NewEmptyPool(t)
			if legacy {
				files, err := filepath.Glob("migrations/*.up.sql")
				require.NoError(t, err)
				for _, file := range files {
					if filepath.Base(file) >= "000014" {
						break
					}
					data, err := os.ReadFile(file)
					require.NoError(t, err)
					_, err = pool.Exec(ctx, string(data))
					require.NoError(t, err)
				}
				createMigrationLedger(t, ctx, pool, 13, false)
				_, err = pool.Exec(ctx, `INSERT INTO archive_file(path,partition_month,row_count,byte_size,runs_path,runs_bytes,series_path,series_bytes) VALUES ('data/legacy.parquet','2024-01-01',42,100,'catalog/runs.parquet',20,'catalog/series.parquet',30)`)
				require.NoError(t, err)
			}
			require.NoError(t, db.Migrate(ctx, pool))
			var active int
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_storage WHERE active`).Scan(&active))
			assert.Equal(t, 1, active)
			var total int
			require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(sum(row_count),0) FROM archive_file`).Scan(&total))
			if legacy {
				assert.Equal(t, 42, total)
			}
			down, err := os.ReadFile("migrations/000014_remote_archive.down.sql")
			require.NoError(t, err)
			_, err = pool.Exec(ctx, string(down))
			require.NoError(t, err, "local legacy state may downgrade")
			up, err := os.ReadFile("migrations/000014_remote_archive.up.sql")
			require.NoError(t, err)
			_, err = pool.Exec(ctx, string(up))
			require.NoError(t, err)
			// A plain import leaves missing receipts, without generating lookup IDs.
			var unverified, locations int
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_object WHERE sha256 IS NULL`).Scan(&unverified))
			if legacy {
				assert.Equal(t, 3, unverified)
			}
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_file_location`).Scan(&locations))
			assert.Zero(t, locations)
			for name, statement := range map[string]string{
				"policy":   `UPDATE archive_policy SET configured_retention_ns=3600000000000`,
				"schedule": `INSERT INTO archive_maintenance(status,import_completed) VALUES ('{"state":"scheduled"}',true)`,
				"upload":   `INSERT INTO archive_cycle(id,owner,state) VALUES ('00000000-0000-0000-0000-000000000001','owner-a','running'); INSERT INTO archive_upload(storage_id,key,cycle_id,expected_size,expected_sha256) SELECT id,'data/pending.parquet','00000000-0000-0000-0000-000000000001',1,decode(repeat('00',32),'hex') FROM archive_storage WHERE active`,
			} {
				t.Run(name, func(t *testing.T) {
					tx, err := pool.Begin(ctx)
					require.NoError(t, err)
					_, err = tx.Exec(ctx, statement)
					require.NoError(t, err)
					_, err = tx.Exec(ctx, string(down))
					require.ErrorContains(t, err, "cannot downgrade")
					require.NoError(t, tx.Rollback(ctx))
				})
			}
			var after int
			require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(sum(row_count),0) FROM archive_file`).Scan(&after))
			assert.Equal(t, total, after)
			_, err = pool.Exec(ctx, `UPDATE archive_storage SET backend='s3',namespace='s3://example-bucket/archive' WHERE active`)
			require.NoError(t, err)
			_, err = pool.Exec(ctx, string(down))
			require.ErrorContains(t, err, "cannot downgrade")
			_, err = pool.Exec(ctx, `UPDATE archive_storage SET backend='file',namespace='' WHERE active; INSERT INTO archive_retirement(storage_id,key) SELECT id,'data/retired.parquet' FROM archive_storage WHERE active`)
			require.NoError(t, err)
			_, err = pool.Exec(ctx, string(down))
			require.ErrorContains(t, err, "cannot downgrade")
		})
	}
}

func TestArchiveStateLookupRanges(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	// No existing ranges may mask a missing encoding constraint with an overlap.
	_, err := pool.Exec(ctx, `INSERT INTO archive_lookup_partition(id,kind,key_encoding,generation) VALUES (gen_random_uuid(),'result','sha256',0)`)
	require.Error(t, err, "result lookups preserve original bytes")
	// Adjacent raw-ID partitions, with explicit unbounded endpoints, are legal.
	_, err = pool.Exec(ctx, `INSERT INTO archive_lookup_partition(id,kind,key_encoding,lower_bound,upper_bound,generation) VALUES
 (gen_random_uuid(),'result','raw-id',NULL,decode('6d','hex'),0),
 (gen_random_uuid(),'result','raw-id',decode('6d','hex'),NULL,0),
 (gen_random_uuid(),'run','sha256',NULL,NULL,0)`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO archive_lookup_partition(id,kind,key_encoding,lower_bound,upper_bound,generation) VALUES (gen_random_uuid(),'result','raw-id',decode('61','hex'),decode('7a','hex'),0)`)
	require.Error(t, err, "overlapping raw-ID ranges must be rejected")
	_, err = pool.Exec(ctx, `INSERT INTO archive_lookup_partition(id,kind,key_encoding,lower_bound,upper_bound,generation) VALUES (gen_random_uuid(),'fingerprint','sha256',decode('7a','hex'),decode('61','hex'),0)`)
	require.Error(t, err, "reversed bounds must be rejected")
	_, err = pool.Exec(ctx, `INSERT INTO archive_lookup_partition(id,kind,key_encoding,lower_bound,upper_bound,generation) VALUES (gen_random_uuid(),'fingerprint','sha256',decode('61','hex'),decode('61','hex'),0)`)
	require.Error(t, err, "empty bounds must be rejected")
}
