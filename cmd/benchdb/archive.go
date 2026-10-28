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
	cmd.AddCommand(newAdminArchiveAuditCommand(stdout))
	cmd.AddCommand(newAdminArchiveTransferCommand(stdout))
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

// adminArchiveConfig names the database and the archive it is bound to.
type adminArchiveConfig struct {
	DatabaseURL string
	Archive     columnar.Config
}

// loadAdminArchiveConfig reads the database URL and archive settings from the
// environment, as the server does.
func loadAdminArchiveConfig(cmd *cobra.Command, args []string, cfg *adminArchiveConfig) error {
	if len(args) > 0 {
		return commandUsageError(cmd, "unexpected argument %q", args[0])
	}
	databaseURL, err := runtimeconfig.DatabaseURL(false)
	if err != nil {
		return err
	}
	archive, err := columnar.ConfigFromEnv()
	if err != nil {
		return commandUsageError(cmd, "%v", err)
	}
	cfg.DatabaseURL, cfg.Archive = databaseURL, archive
	return nil
}

// withDatabase connects to the database for one admin operation.
func withDatabase(ctx context.Context, cfg adminArchiveConfig, fn func(*pgxpool.Pool) error) error {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("connect database failed")
	}
	defer pool.Close()
	pingCtx, cancel := context.WithTimeout(ctx, recoverStagingConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		return errors.New("connect database failed")
	}
	return fn(pool)
}

// withArchive opens the archive engine for one admin operation.
func withArchive(ctx context.Context, pool *pgxpool.Pool, cfg adminArchiveConfig, fn func(*columnar.Engine) error) error {
	opening, cancel := context.WithTimeout(ctx, archiveOpenTimeout)
	engine, err := columnar.Open(opening, pool, cfg.Archive)
	cancel()
	if err != nil {
		return fmt.Errorf("open managed archive: %w", err)
	}
	return errors.Join(fn(engine), engine.Close())
}

// archiveOpenTimeout bounds archive startup, which checks the storage binding.
const archiveOpenTimeout = 2 * time.Minute

func newAdminArchiveAuditCommand(stdout io.Writer) *cobra.Command {
	cfg := adminArchiveConfig{}
	return configureCommand(&cobra.Command{
		Use:   "audit",
		Short: "Check that every object the archive manifest references exists.",
		Long: "Request the metadata of every fact file and catalog that the database's\n" +
			"manifest and lookup directory reference in the configured archive, and\n" +
			"compare each object's size and revision with its receipt. Startup checks only\n" +
			"the dataset marker of a remote archive; run the audit after a restore or a\n" +
			"transfer, before serving. The command fails if any object is missing or changed.",
		Args: func(cmd *cobra.Command, args []string) error {
			return loadAdminArchiveConfig(cmd, args, &cfg)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDatabase(cmd.Context(), cfg, func(pool *pgxpool.Pool) error {
				return withArchive(cmd.Context(), pool, cfg, func(engine *columnar.Engine) error {
					result, err := engine.AuditArchive(cmd.Context())
					if _, writeErr := fmt.Fprintf(stdout, "Objects checked: %d\nBytes referenced: %d\nMissing: %d\nChanged: %d\n",
						result.Objects, result.Bytes, len(result.Missing), len(result.Changed)); writeErr != nil {
						return errors.Join(err, writeErr)
					}
					return err
				})
			})
		},
	})
}

func newAdminArchiveTransferCommand(stdout io.Writer) *cobra.Command {
	cfg := adminArchiveConfig{}
	destination := columnar.Config{}
	cmd := configureCommand(&cobra.Command{
		Use:   "transfer --destination <url>",
		Short: "Copy a stopped deployment's archive to new storage and switch the database to it.",
		Long: "Copy every fact file and catalog that the database references from the\n" +
			"configured archive to the destination, verify each copy's SHA-256, and switch\n" +
			"the database's archive binding to the destination in one transaction. The\n" +
			"destination must be empty, or hold a transfer from this database that was\n" +
			"interrupted; running the command again resumes it without copying verified\n" +
			"objects again. The source archive is kept: backups of the database still use it.\n\n" +
			"Stop every BenchDB server and admin process first. The command takes archive\n" +
			"maintenance ownership and fails while another process holds it. After it\n" +
			"succeeds, set BENCHDB_ARCHIVE_URL and its provider settings to the destination\n" +
			"and start the server.",
		Args: func(cmd *cobra.Command, args []string) error {
			if destination.ArchiveURL == "" {
				return commandUsageError(cmd, "--destination is required")
			}
			return loadAdminArchiveConfig(cmd, args, &cfg)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDatabase(cmd.Context(), cfg, func(pool *pgxpool.Pool) error {
				// After a committed cutover the source settings no longer open
				// the archive, so a rerun reads the outcome first.
				done, completed, err := columnar.CompletedTransfer(cmd.Context(), pool, destination)
				if err != nil {
					return err
				}
				if completed {
					_, err = fmt.Fprintf(stdout, "Transfer: %s\nThe transfer already completed. The database uses %s; "+
						"set BENCHDB_ARCHIVE_URL to it before starting BenchDB.\n", done.ID, destination.ArchiveURL)
					return err
				}
				return withArchive(cmd.Context(), pool, cfg, func(engine *columnar.Engine) error {
					result, err := engine.TransferArchive(cmd.Context(), destination)
					if err != nil {
						return err
					}
					_, err = fmt.Fprintf(stdout, "Transfer: %s\nObjects: %d\nBytes: %d\nCopied: %d\nAlready verified: %d\n"+
						"The database now uses %s. Set BENCHDB_ARCHIVE_URL to it before starting BenchDB.\n",
						result.ID, result.Objects, result.Bytes, result.Copied, result.Reused, destination.ArchiveURL)
					return err
				})
			})
		},
	})
	cmd.Flags().StringVar(&destination.ArchiveURL, "destination", "", "destination archive URL: file:///path, s3://bucket/prefix, or gs://bucket/prefix")
	cmd.Flags().StringVar(&destination.S3Endpoint, "destination-s3-endpoint", "", "S3-compatible endpoint origin for an s3:// destination")
	cmd.Flags().StringVar(&destination.S3Region, "destination-s3-region", "", "region for an s3:// destination (default us-east-1)")
	cmd.Flags().StringVar(&destination.S3URLStyle, "destination-s3-url-style", "", "path or vhost addressing for an s3:// destination")
	cmd.Flags().StringVar(&destination.GCSCredentialsFile, "destination-gcs-credentials-file", "", "Google credentials JSON file for a gs:// destination")
	return cmd
}
