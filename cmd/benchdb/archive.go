package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"go.kenn.io/benchdb/internal/columnar"
	"go.kenn.io/benchdb/internal/runtimeconfig"
)

type adminArchiveRecoverStagingConfig struct {
	DatabaseURL string
	Archive     columnar.Config
}

var runAdminArchiveRecoverStaging = runAdminArchiveRecoverStagingReal

// recoverStagingConnectTimeout bounds the connection check, so an unreachable
// or stalled database fails the command instead of hanging it.
const recoverStagingConnectTimeout = 10 * time.Second

func adminArchiveCommand(stdout io.Writer) *cobra.Command {
	cmd := groupCommand("archive", "Run managed archive operations.")
	cmd.AddCommand(newAdminArchiveRecoverStagingCommand(stdout, runAdminArchiveRecoverStagingConfig))
	return cmd
}

func newAdminArchiveRecoverStagingCommand(
	stdout io.Writer,
	run func(context.Context, adminArchiveRecoverStagingConfig, io.Writer) error,
) *cobra.Command {
	cfg := adminArchiveRecoverStagingConfig{}
	return configureCommand(&cobra.Command{
		Use:   "recover-staging",
		Short: "Remove staging files that a crashed publisher left in the local archive.",
		Long: "Remove .objectstore-* staging files from the local archive directory: the\n" +
			"file:// BENCHDB_ARCHIVE_URL, or BENCHDB_DATA_DIR without one. S3 and GCS\n" +
			"archives have no such files, and the command refuses them.\n\n" +
			"The command takes archive maintenance ownership, which proves that no BenchDB\n" +
			"publisher is running, and fails if another process holds it. Stop every other\n" +
			"process that publishes into the archive before you run it.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return commandUsageError(cmd, "unexpected argument %q", args[0])
			}
			databaseURL, err := runtimeconfig.DatabaseURL(false)
			if err != nil {
				return err
			}
			cfg.DatabaseURL = databaseURL
			archive, err := columnar.ConfigFromEnv()
			if err != nil {
				return commandUsageError(cmd, "%v", err)
			}
			cfg.Archive = archive
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), cfg, stdout)
		},
	})
}

func runAdminArchiveRecoverStagingConfig(
	ctx context.Context, cfg adminArchiveRecoverStagingConfig, stdout io.Writer,
) error {
	result, err := runAdminArchiveRecoverStaging(ctx, cfg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Staging files removed: %d\nBytes reclaimed: %d\n", result.Files, result.Bytes)
	return err
}

func runAdminArchiveRecoverStagingReal(ctx context.Context, cfg adminArchiveRecoverStagingConfig) (columnar.StagingRecovery, error) {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return columnar.StagingRecovery{}, errors.New("connect database failed")
	}
	defer pool.Close()
	pingCtx, cancel := context.WithTimeout(ctx, recoverStagingConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		return columnar.StagingRecovery{}, errors.New("connect database failed")
	}
	return columnar.RecoverStaging(ctx, pool, cfg.Archive)
}
