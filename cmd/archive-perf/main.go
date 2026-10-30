// Command archive-perf measures the latency of BenchDB's archive reads
// against one database and its archive. It is an opt-in operator tool, not a
// test: it connects read-only, refuses a database whose dataset identity
// differs from --expect-dataset, and never migrates, exports, or writes.
//
//	archive-perf inputs --database-url URL --expect-dataset ID --output inputs.json
//	archive-perf run --database-url URL --expect-dataset ID --inputs inputs.json \
//	    --scenario result --variant catalog --concurrency 4 --samples 30 --output result.json
//
// Archive and DuckDB settings come from the server's BENCHDB_* environment;
// --data-dir and --archive-url override BENCHDB_DATA_DIR and
// BENCHDB_ARCHIVE_URL. Inputs are generated separately from the run, so
// every variant measures the same frozen keys and their answers can be
// compared by answer_sha256.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/pflag"

	"go.kenn.io/benchdb/internal/columnar"
	"go.kenn.io/benchdb/internal/db"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

const usage = `usage: archive-perf inputs|run [flags]

  inputs  sample runs and derive frozen result IDs and histories
  run     time one scenario over the frozen inputs

Run "archive-perf <command> --help" for its flags.
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "inputs":
		err = runInputs(ctx, args[1:], stdout, stderr)
	case "run":
		err = runMeasure(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n%s", args[0], usage)
		return 2
	}
	if errors.Is(err, pflag.ErrHelp) {
		return 0
	}
	if usageErr, ok := errors.AsType[usageError](err); ok {
		_, _ = fmt.Fprintf(stderr, "archive-perf: %s\n", usageErr)
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archive-perf: %v\n", err)
		return 1
	}
	return 0
}

type usageError string

func (e usageError) Error() string { return string(e) }

// target names the database and archive one command measures.
type target struct {
	databaseURL, expectDataset, dataDir, archiveURL string
}

func (t *target) register(flags *pflag.FlagSet) {
	flags.StringVar(&t.databaseURL, "database-url", "", "PostgreSQL URL of the database to measure (required)")
	flags.StringVar(&t.expectDataset, "expect-dataset", "", "archive_dataset ID the database must have (required)")
	flags.StringVar(&t.dataDir, "data-dir", "", "working directory; overrides BENCHDB_DATA_DIR")
	flags.StringVar(&t.archiveURL, "archive-url", "", "archive root; overrides BENCHDB_ARCHIVE_URL")
}

func (t *target) check() error {
	if t.databaseURL == "" || t.expectDataset == "" {
		return usageError("--database-url and --expect-dataset are required")
	}
	return nil
}

// session is an open, read-only connection to the target's database and
// archive.
type session struct {
	pool   *pgxpool.Pool
	engine *columnar.Engine
	store  *columnar.Store
}

func (s *session) close() error {
	err := s.engine.Close()
	s.pool.Close()
	return err
}

// open connects read-only, checks the dataset identity, and opens the archive
// with the server's settings. The read-only option travels in the URL, so
// the archive's coordination sessions and DuckDB's PostgreSQL connections
// inherit it too.
func (t *target) open(ctx context.Context) (*session, error) {
	readOnly, err := readOnlyURL(t.databaseURL)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, readOnly)
	if err != nil {
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	if err := t.verify(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	cfg, err := t.archiveConfig()
	if err != nil {
		pool.Close()
		return nil, usageError(err.Error())
	}
	// Archive coordination can use its own URL; it must be read-only too, or
	// startup could claim the storage binding through it.
	if cfg.CoordinationDBURL != "" {
		if cfg.CoordinationDBURL, err = readOnlyURL(cfg.CoordinationDBURL); err != nil {
			pool.Close()
			return nil, err
		}
		if err := checkReadOnly(ctx, cfg.CoordinationDBURL); err != nil {
			pool.Close()
			return nil, err
		}
	}
	engine, err := columnar.Open(ctx, pool, cfg)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("open the archive: %w", err)
	}
	return &session{pool: pool, engine: engine, store: columnar.NewStore(db.NewStore(pool), engine)}, nil
}

func readOnlyURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", usageError("--database-url must be a postgres:// URL")
	}
	query := u.Query()
	options := query.Get("options")
	if options != "" {
		options += " "
	}
	query.Set("options", options+"-c default_transaction_read_only=on")
	// PostgreSQL clients read a space as %20, not +. Encode escapes a literal
	// + as %2B, so every remaining + is a space.
	u.RawQuery = strings.ReplaceAll(query.Encode(), "+", "%20")
	return u.String(), nil
}

// verify refuses a session that is not read-only or a database whose dataset
// identity differs from the expected one.
func (t *target) verify(ctx context.Context, pool *pgxpool.Pool) error {
	var readOnly, dataset string
	err := pool.QueryRow(ctx, `SELECT current_setting('default_transaction_read_only'),
(SELECT id::text FROM archive_dataset WHERE singleton)`).Scan(&readOnly, &dataset)
	if err != nil {
		return fmt.Errorf("read the database's identity: %w", err)
	}
	if readOnly != "on" {
		return errors.New("the database session is not read-only")
	}
	if dataset != t.expectDataset {
		return fmt.Errorf("the database's dataset is %s, not the expected %s", dataset, t.expectDataset)
	}
	return nil
}

// checkReadOnly confirms that sessions opened with url are read-only.
func checkReadOnly(ctx context.Context, url string) error {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return fmt.Errorf("connect to the archive coordination database: %w", err)
	}
	defer pool.Close()
	var readOnly string
	if err := pool.QueryRow(ctx, `SELECT current_setting('default_transaction_read_only')`).Scan(&readOnly); err != nil {
		return fmt.Errorf("read the archive coordination session's settings: %w", err)
	}
	if readOnly != "on" {
		return errors.New("the archive coordination session is not read-only")
	}
	return nil
}

func (t *target) archiveConfig() (columnar.Config, error) {
	for name, value := range map[string]string{"BENCHDB_DATA_DIR": t.dataDir, "BENCHDB_ARCHIVE_URL": t.archiveURL} {
		if value == "" {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			return columnar.Config{}, err
		}
	}
	return columnar.ConfigFromEnv()
}
