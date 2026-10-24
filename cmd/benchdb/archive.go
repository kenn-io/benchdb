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
		Long: "Remove .objectstore-* staging files from the local archive directory.\n\n" +
			"The command takes archive maintenance ownership, which proves that no BenchDB\n" +
			"publisher is running, and fails if another process holds it. Stop every other\n" +
			"process that publishes into BENCHDB_DATA_DIR before you run it.",
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
	if err := pool.Ping(ctx); err != nil {
		return columnar.StagingRecovery{}, errors.New("connect database failed")
	}
	opening, cancel := context.WithTimeout(ctx, 30*time.Second)
	engine, err := columnar.Open(opening, pool, cfg.Archive)
	cancel()
	if err != nil {
		return columnar.StagingRecovery{}, fmt.Errorf("open managed archive: %w", err)
	}
	defer engine.Close()
	return engine.RecoverStaging(ctx)
}
