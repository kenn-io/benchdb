package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/runtimeconfig"
)

var runMigrate = runMigrateReal

func migrateCommand(stdout io.Writer) *cobra.Command {
	return configureCommand(&cobra.Command{
		Use:   "migrate",
		Short: "Apply database schema migrations.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return commandUsageError(cmd, "unexpected argument %q", args[0])
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			databaseURL, err := runtimeconfig.DatabaseURL(true)
			if err != nil {
				return err
			}
			if err := runMigrate(cmd.Context(), databaseURL); err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, "database schema is current")
			return err
		},
	})
}

func runMigrateReal(ctx context.Context, databaseURL string) error {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return errors.New("invalid database configuration")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return db.Migrate(ctx, pool)
}
