package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/runtimeconfig"
	"go.kenn.io/benchdb/internal/service"
)

type adminAlertsSyncConfig struct {
	DatabaseURL string
	File        string
	Email       string
	UserName    string
	Rules       []service.AlertRuleSpec
}

// alertRulesFile is the --file format: the complete rule list for one owner.
type alertRulesFile struct {
	Rules []service.AlertRuleSpec `json:"rules"`
}

type adminAlertsSyncOutput struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	service.AlertRulesSynced
}

var runAdminAlertsSync = runAdminAlertsSyncReal

func adminAlertsSyncCommand(stdout, stderr io.Writer) *cobra.Command {
	return newAdminAlertsSyncCommand(stdout, stderr, runAdminAlertsSyncConfig)
}

func newAdminAlertsSyncCommand(
	stdout, stderr io.Writer,
	run func(context.Context, adminAlertsSyncConfig, io.Writer, io.Writer) error,
) *cobra.Command {
	cfg := adminAlertsSyncConfig{}
	cmd := configureCommand(&cobra.Command{
		Use:   "sync",
		Short: "Make one owner's alert rules match a rules file.",
		Long: "Creates, updates, and deletes the owner's alert rules so they match --file exactly, " +
			"matching rules by name. Use an owner account whose rules come only from this file. " +
			"The owner is created if it does not exist.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return commandUsageError(cmd, "unexpected argument %q", args[0])
			}
			email, err := normalizeAdminTokenEmail(cfg.Email)
			if err != nil {
				return commandUsageError(cmd, "%s", err)
			}
			cfg.Email = email
			cfg.UserName = strings.TrimSpace(cfg.UserName)
			if cfg.UserName == "" {
				cfg.UserName = cfg.Email
			}
			if strings.TrimSpace(cfg.File) == "" {
				return commandUsageError(cmd, "--file is required")
			}
			rules, err := readAlertRulesFile(cfg.File)
			if err != nil {
				return commandUsageError(cmd, "%s", err)
			}
			cfg.Rules = rules
			databaseURL, dbErr := runtimeconfig.DatabaseURL(false)
			if dbErr != nil {
				return dbErr
			}
			cfg.DatabaseURL = databaseURL
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), cfg, stdout, stderr)
		},
	})
	cmd.Flags().StringVar(&cfg.File, "file", "", `JSON file of the form {"rules": [...]}`)
	cmd.Flags().StringVar(&cfg.Email, "email", "", "email of the account that owns the rules")
	cmd.Flags().StringVar(&cfg.UserName, "user-name", "", "display name for a newly created owner")
	return cmd
}

func readAlertRulesFile(path string) ([]service.AlertRuleSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --file: %w", err)
	}
	var file alertRulesFile
	if err := json.Unmarshal(raw, &file, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("parse --file %s: %w", path, err)
	}
	if file.Rules == nil {
		return nil, fmt.Errorf("parse --file %s: missing \"rules\" list", path)
	}
	return file.Rules, nil
}

func runAdminAlertsSyncConfig(
	ctx context.Context, cfg adminAlertsSyncConfig, stdout, stderr io.Writer,
) error {
	out, err := runAdminAlertsSync(ctx, cfg, stdout, stderr)
	if err != nil {
		return err
	}
	return writeJSONLine(stdout, out)
}

func runAdminAlertsSyncReal(
	ctx context.Context, cfg adminAlertsSyncConfig, _, _ io.Writer,
) (adminAlertsSyncOutput, error) {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return adminAlertsSyncOutput{}, errors.New("connect database failed")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return adminAlertsSyncOutput{}, errors.New("connect database failed")
	}
	store := db.NewStore(pool)
	userID, err := store.GetOrCreateUserByEmail(ctx, cfg.Email, cfg.UserName, "!")
	if err != nil {
		return adminAlertsSyncOutput{}, fmt.Errorf("create owner: %w", err)
	}
	var synced service.AlertRulesSynced
	err = store.WithAlertRuleLock(ctx, userID, func(tx *db.Store) error {
		synced, err = service.SyncAlertRules(ctx, tx, userID, cfg.Rules, time.Now().UTC())
		return err
	})
	if err != nil {
		return adminAlertsSyncOutput{}, fmt.Errorf("sync alert rules: %w", err)
	}
	return adminAlertsSyncOutput{UserID: userID, Email: cfg.Email, AlertRulesSynced: synced}, nil
}
