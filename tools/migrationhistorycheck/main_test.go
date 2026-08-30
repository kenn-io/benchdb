package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunRejectsCommittedEditToBaseMigration(t *testing.T) {
	repoDir := t.TempDir()
	configDir := t.TempDir()
	for _, key := range []string{
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_COMMON_DIR",
		"GIT_DIR",
		"GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY",
		"GIT_WORK_TREE",
	} {
		value, exists := os.LookupEnv(key)
		require.NoError(t, os.Unsetenv(key))
		t.Cleanup(func() {
			if exists {
				require.NoError(t, os.Setenv(key, value))
			} else {
				require.NoError(t, os.Unsetenv(key))
			}
		})
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(configDir, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	migrationDir := filepath.Join(repoDir, defaultMigrationDir)
	require.NoError(t, os.MkdirAll(migrationDir, 0o700))
	upPath := filepath.Join(migrationDir, "000001_initial.up.sql")
	downPath := filepath.Join(migrationDir, "000001_initial.down.sql")
	require.NoError(t, os.WriteFile(upPath, []byte("SELECT 1;\n"), 0o600))
	require.NoError(t, os.WriteFile(downPath, []byte("SELECT 1;\n"), 0o600))

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
	}
	git("init", "--quiet")
	git("add", ".")
	git("-c", "user.name=Test User", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "initial")
	require.NoError(t, os.WriteFile(upPath, []byte("SELECT 2;\n"), 0o600))
	git("add", ".")
	git("-c", "user.name=Test User", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "edit migration")

	originalDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(repoDir))
	t.Cleanup(func() { require.NoError(t, os.Chdir(originalDir)) })
	t.Setenv("BENCHDB_MIGRATION_BASE_REF", "HEAD^")
	t.Setenv("GITHUB_REF", "")

	err = run(context.Background())
	require.ErrorContains(t, err, "already exists on the comparison base")
}

func TestValidateMigrationHistoryAllowsBootstrapFromEmptyHistory(t *testing.T) {
	err := validateMigrationHistory(nil, []string{
		"internal/db/migrations/000001_initial_schema.up.sql",
		"internal/db/migrations/000001_initial_schema.down.sql",
		"internal/db/migrations/000002_submission_idempotency.up.sql",
		"internal/db/migrations/000002_submission_idempotency.down.sql",
	}, nil)
	assert.NoError(t, err)
}

func TestValidateMigrationHistoryAllowsOneMigrationAfterBootstrap(t *testing.T) {
	base := []string{
		"internal/db/migrations/000001_initial_schema.up.sql",
		"internal/db/migrations/000001_initial_schema.down.sql",
	}
	candidate := append(append([]string(nil), base...),
		"internal/db/migrations/000002_next.up.sql",
		"internal/db/migrations/000002_next.down.sql",
	)
	assert.NoError(t, validateMigrationHistory(base, candidate, nil))
}

func TestValidateMigrationHistoryAllowsOneTimeBaselineReset(t *testing.T) {
	base := []string{
		"internal/db/migrations/000001_initial_schema.up.sql",
		"internal/db/migrations/000001_initial_schema.down.sql",
		"internal/db/migrations/000002_submission_idempotency.up.sql",
		"internal/db/migrations/000002_submission_idempotency.down.sql",
	}
	candidate := []string{
		"internal/db/migrations/000001_initial_schema.up.sql",
		"internal/db/migrations/000001_initial_schema.down.sql",
		baselineResetMarker,
	}
	changed := []string{
		"internal/db/migrations/000001_initial_schema.up.sql",
		"internal/db/migrations/000002_submission_idempotency.up.sql",
		"internal/db/migrations/000002_submission_idempotency.down.sql",
		baselineResetMarker,
	}
	assert.NoError(t, validateMigrationHistory(base, candidate, changed))
}

func TestValidateMigrationHistoryRejectsSecondBaselineReset(t *testing.T) {
	base := []string{
		"internal/db/migrations/000001_initial_schema.up.sql",
		"internal/db/migrations/000001_initial_schema.down.sql",
		baselineResetMarker,
	}
	err := validateMigrationHistory(base, base, []string{"internal/db/migrations/000001_initial_schema.up.sql"})
	assert.ErrorContains(t, err, "already exists on the comparison base")
}

func TestValidateMigrationHistoryRequiresSinglePairAfterBaselineReset(t *testing.T) {
	candidate := []string{
		"internal/db/migrations/000001_initial_schema.up.sql",
		"internal/db/migrations/000001_initial_schema.down.sql",
		"internal/db/migrations/000002_extra.up.sql",
		"internal/db/migrations/000002_extra.down.sql",
		baselineResetMarker,
	}
	err := validateMigrationHistory(nil, candidate, []string{baselineResetMarker})
	assert.ErrorContains(t, err, "exactly one 000001 migration pair")
}

func TestValidateMigrationHistoryRejectsMultipleNewMigrations(t *testing.T) {
	base := []string{
		"internal/db/migrations/000001_initial_schema.up.sql",
		"internal/db/migrations/000001_initial_schema.down.sql",
	}
	candidate := append(append([]string(nil), base...),
		"internal/db/migrations/000002_first.up.sql",
		"internal/db/migrations/000002_first.down.sql",
		"internal/db/migrations/000003_second.up.sql",
		"internal/db/migrations/000003_second.down.sql",
	)
	err := validateMigrationHistory(base, candidate, nil)
	assert.ErrorContains(t, err, "only one new migration")
}

func TestValidateMigrationHistoryRejectsChangedBaseMigration(t *testing.T) {
	base := []string{
		"internal/db/migrations/000001_initial_schema.up.sql",
		"internal/db/migrations/000001_initial_schema.down.sql",
	}
	err := validateMigrationHistory(base, base, []string{"internal/db/migrations/000001_initial_schema.up.sql"})
	assert.ErrorContains(t, err, "already exists on the comparison base")
}

func TestValidateMigrationHistoryRequiresUpAndDownPair(t *testing.T) {
	err := validateMigrationHistory(nil, []string{
		"internal/db/migrations/000001_initial_schema.up.sql",
	}, nil)
	assert.ErrorContains(t, err, "must have matching .up.sql and .down.sql files")
}

func TestValidateMigrationHistoryRejectsDuplicateNumber(t *testing.T) {
	err := validateMigrationHistory(nil, []string{
		"internal/db/migrations/000001_first.up.sql",
		"internal/db/migrations/000001_first.down.sql",
		"internal/db/migrations/000001_second.up.sql",
		"internal/db/migrations/000001_second.down.sql",
	}, nil)
	assert.ErrorContains(t, err, "migration number 000001")
}

func TestValidateMigrationHistoryRequiresContiguousNumbers(t *testing.T) {
	err := validateMigrationHistory(nil, []string{
		"internal/db/migrations/000001_initial.up.sql",
		"internal/db/migrations/000001_initial.down.sql",
		"internal/db/migrations/000003_later.up.sql",
		"internal/db/migrations/000003_later.down.sql",
	}, nil)
	assert.ErrorContains(t, err, "expected migration 000002")
}
