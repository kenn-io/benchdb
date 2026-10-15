package columnar

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/storage"
)

func TestMetadataReadsDoNotWaitForArchive(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	_, err := pool.Exec(ctx, `
INSERT INTO "commit" (id,sha,parent,repository,message,author_name,"timestamp",fork_point_sha) VALUES
('root-id','root','foreign','https://example.com/repo','root message','fixture','2024-01-01','root'),
('parent-id','parent','root','https://example.com/repo','parent message','fixture','2024-01-02','parent'),
('head-id','head','parent','https://example.com/repo','head message','fixture','2024-01-03','parent'),
('foreign-id','foreign',NULL,'https://example.com/other','foreign message','fixture','2024-01-04','foreign');
INSERT INTO result_submission (submission_key,result_id,run_id,history_fingerprint,payload_sha256)
VALUES ('submission-key','result-id','run-id','fingerprint',repeat('a',64));
INSERT INTO run_verdict (run_id,repository,last_result_at,last_result_id,default_branch,needs_attention,attention,computed_at)
VALUES ('run-id','https://example.com/repo','2024-01-03','result-id',false,true,'{"status":"regression"}','2024-01-04');
DELETE FROM verdict_queue;
INSERT INTO verdict_queue (kind,key) VALUES ('commit','head-id');`)
	require.NoError(t, err)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	store := NewStore(db.NewStore(pool), e)
	const repository = "https://example.com/repo"
	head := storage.CIReportCommitRow{
		CommitID: "head-id", CommitSha: "head", Repository: repository,
		Parent: new("parent"), ForkPointSha: new("parent"),
		Timestamp: new(time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)), Message: "head message",
	}
	parent := storage.CIReportCommitRow{
		CommitID: "parent-id", CommitSha: "parent", Repository: repository,
		Parent: new("root"), ForkPointSha: new("parent"),
		Timestamp: new(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)), Message: "parent message",
	}
	root := storage.CIReportCommitRow{
		CommitID: "root-id", CommitSha: "root", Repository: repository,
		Parent: new("foreign"), ForkPointSha: new("root"),
		Timestamp: new(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)), Message: "root message",
	}
	checks := []struct {
		name string
		read func(*testing.T, context.Context)
	}{
		{"submission", func(t *testing.T, ctx context.Context) {
			result, err := store.GetBenchmarkResultBySubmissionKey(ctx, "submission-key")
			require.NoError(t, err)
			assert.Equal(t, storage.SubmissionResult{ID: "result-id", RunID: "run-id", HistoryFingerprint: "fingerprint",
				PayloadSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, result)
			_, err = store.GetBenchmarkResultBySubmissionKey(ctx, "missing")
			require.ErrorIs(t, err, storage.ErrNotFound)
		}},
		{"commit", func(t *testing.T, ctx context.Context) {
			commit, err := store.GetCIReportCommit(ctx, repository, "head")
			require.NoError(t, err)
			assert.Equal(t, head, commit)
			_, err = store.GetCIReportCommit(ctx, repository, "foreign")
			require.ErrorIs(t, err, storage.ErrNotFound)
		}},
		{"latest-default-commit", func(t *testing.T, ctx context.Context) {
			commit, err := store.SelectLatestDefaultCommit(ctx, repository)
			require.NoError(t, err)
			assert.Equal(t, parent, commit)
		}},
		{"ancestry", func(t *testing.T, ctx context.Context) {
			commits, err := store.SelectCIReportBaselineAncestry(ctx, repository, "head", 2)
			require.NoError(t, err)
			assert.Equal(t, []storage.CIReportCommitRow{head, parent}, commits)
			commits, err = store.SelectCIReportBaselineAncestry(ctx, repository, "head", 20)
			require.NoError(t, err)
			assert.Equal(t, []storage.CIReportCommitRow{head, parent, root}, commits)
			commits, err = store.SelectCIReportBaselineAncestry(ctx, repository, "head", 0)
			require.NoError(t, err)
			assert.Empty(t, commits)
		}},
		{"run-verdicts", func(t *testing.T, ctx context.Context) {
			verdicts, err := store.SelectRunVerdicts(ctx, []string{"run-id", "missing"})
			require.NoError(t, err)
			require.Len(t, verdicts, 1)
			assert.Equal(t, "run-id", verdicts[0].RunID)
			assert.Equal(t, repository, verdicts[0].Repository)
			assert.True(t, verdicts[0].NeedsAttention)
			assert.True(t, verdicts[0].Pending)
			assert.JSONEq(t, `{"status":"regression"}`, string(verdicts[0].Attention))
		}},
		{"pending", func(t *testing.T, ctx context.Context) {
			pending, err := store.VerdictsPending(ctx, new(repository))
			require.NoError(t, err)
			assert.True(t, pending)
			pending, err = store.VerdictsPending(ctx, new("https://example.com/other"))
			require.NoError(t, err)
			assert.False(t, pending)
		}},
		{"attention", func(t *testing.T, ctx context.Context) {
			count, err := store.CountAttentionRuns(ctx, new(repository))
			require.NoError(t, err)
			assert.EqualValues(t, 1, count)
			count, err = store.CountAttentionRuns(ctx, new("https://example.com/other"))
			require.NoError(t, err)
			assert.Zero(t, count)
		}},
	}
	check := func(t *testing.T) {
		t.Helper()
		// Confirm fact reads really are blocked before testing metadata reads.
		blocked, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_, err := store.CountBenchmarkResults(blocked)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		for _, check := range checks {
			t.Run(check.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				check.read(t, ctx)
			})
		}
	}

	t.Run("occupied-reader", func(t *testing.T) {
		ready, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- e.Read(ctx, func(*sql.Tx) error {
				close(ready)
				<-release
				return nil
			})
		}()
		t.Cleanup(func() {
			close(release)
			require.NoError(t, <-done)
		})
		select {
		case <-ready:
		case err := <-done:
			done <- err
			require.FailNow(t, "occupy archive reader", "%v", err)
		}
		check(t)
	})
	t.Run("cleanup-admission", func(t *testing.T) {
		gate := flock.New(filepath.Join(e.cfg.Dir, ".readers-admission.lock"))
		require.NoError(t, gate.Lock())
		t.Cleanup(func() { require.NoError(t, gate.Close()) })
		pending := filepath.Join(e.cfg.Dir, ".cleanup-pending")
		require.NoError(t, os.WriteFile(pending, nil, 0600))
		t.Cleanup(func() { require.NoError(t, os.Remove(pending)) })
		check(t)
	})
}
