package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/pflag"

	"go.kenn.io/benchdb/internal/storage"
)

// scenario reads one answer for request i over the frozen inputs. key names
// the input, so answers to the same key can be compared.
type scenario func(ctx context.Context, s *session, in inputs, i int) (key string, answer any, err error)

func pick(values []string, i int) string { return values[i%len(values)] }

var scenarios = map[string]scenario{
	"result": func(ctx context.Context, s *session, in inputs, i int) (string, any, error) {
		id := pick(in.Results, i)
		row, err := s.store.GetBenchmarkResultDetail(ctx, id)
		return id, row, err
	},
	"result-miss": func(ctx context.Context, s *session, _ inputs, i int) (string, any, error) {
		id := fmt.Sprintf("archive-perf-missing-%d", i)
		_, err := s.store.GetBenchmarkResultDetail(ctx, id)
		if errors.Is(err, storage.ErrNotFound) {
			return "missing", "not found", nil
		}
		if err == nil {
			err = errors.New("a missing result was found")
		}
		return "missing", nil, err
	},
	"run-results": func(ctx context.Context, s *session, in inputs, i int) (string, any, error) {
		run := pick(in.Runs, i)
		ids, err := s.store.SelectRunResultIDs(ctx, run)
		slices.Sort(ids)
		return run, ids, err
	},
	"run-verdict-subjects": func(ctx context.Context, s *session, in inputs, i int) (string, any, error) {
		run := pick(in.Runs, i)
		subjects, err := s.store.SelectRunVerdictSubjects(ctx, run)
		return run, subjects, err
	},
	"history": func(ctx context.Context, s *session, in inputs, i int) (string, any, error) {
		fingerprint := pick(in.Fingerprints, i)
		rows, err := s.store.SelectHistoryForFingerprint(ctx, fingerprint)
		return fingerprint, rows, err
	},
	"recent-runs": func(ctx context.Context, s *session, _ inputs, _ int) (string, any, error) {
		rows, err := s.store.SelectRecentRuns(ctx, storage.RecentRunsParams{PageSize: 50})
		return "recent", rows, err
	},
}

// measurement is one run's output. Latencies cover successful requests only:
// an error or timeout is counted, never timed as a fast answer.
type measurement struct {
	Scenario       string  `json:"scenario"`
	Variant        string  `json:"variant"`
	Dataset        string  `json:"dataset"`
	Backend        string  `json:"backend"`
	Concurrency    int     `json:"concurrency"`
	Warmup         int     `json:"warmup"`
	Samples        int     `json:"samples"`
	Succeeded      int     `json:"succeeded"`
	Errors         int     `json:"errors"`
	Timeouts       int     `json:"timeouts"`
	P50Millis      float64 `json:"p50_ms"`
	P95Millis      float64 `json:"p95_ms"`
	MaxMillis      float64 `json:"max_ms"`
	AnswerSHA256   string  `json:"answer_sha256"`
	Inconsistent   int     `json:"inconsistent_answers"`
	PeakBackends   int     `json:"peak_backends"`
	FirstError     string  `json:"first_error,omitempty"`
	StartedAt      string  `json:"started_at"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
}

func runMeasure(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := pflag.NewFlagSet("archive-perf run", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	var t target
	t.register(flags)
	inputsPath := flags.String("inputs", "", "inputs JSON from archive-perf inputs (required)")
	name := flags.String("scenario", "", "one of: "+strings.Join(slices.Sorted(maps.Keys(scenarios)), ", ")+" (required)")
	variant := flags.String("variant", "", "postgres (nothing archived) or catalog (complete lookup catalogs) (required)")
	concurrency := flags.Int("concurrency", 1, "concurrent requests")
	samples := flags.Int("samples", 30, "timed requests")
	warmup := flags.Int("warmup", 0, "untimed requests first; 0 measures a cold cache")
	output := flags.String("output", "", "file for the result JSON; stdout when empty")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := t.check(); err != nil {
		return err
	}
	read, ok := scenarios[*name]
	if !ok || *inputsPath == "" || *concurrency <= 0 || *samples <= 0 || *warmup < 0 {
		return usageError("--inputs, a known --scenario, positive --concurrency and --samples, and a non-negative --warmup are required")
	}
	frozen, err := readInputs(*inputsPath, t.expectDataset)
	if err != nil {
		return err
	}
	s, err := t.open(ctx)
	if err != nil {
		return err
	}
	result := measurement{Scenario: *name, Variant: *variant, Dataset: t.expectDataset, Concurrency: *concurrency, Warmup: *warmup, Samples: *samples}
	measureErr := measure(ctx, s, frozen, read, &result)
	if err := errors.Join(measureErr, s.close()); err != nil {
		return err
	}
	return writeJSON(stdout, *output, result)
}

func measure(ctx context.Context, s *session, frozen inputs, read scenario, result *measurement) error {
	if err := checkVariant(ctx, s, result.Variant); err != nil {
		return err
	}
	status, err := s.store.StorageStatus(ctx)
	if err != nil {
		return err
	}
	result.Backend = status.Backend
	for i := range result.Warmup {
		if _, _, err := read(ctx, s, frozen, i); err != nil {
			return fmt.Errorf("warm-up request %d: %w", i, err)
		}
	}
	peak, stopPeak := watchBackends(ctx, s)
	started := time.Now()
	result.StartedAt = started.UTC().Format(time.RFC3339)
	outcomes := collect(ctx, s, frozen, read, result.Concurrency, result.Samples)
	result.ElapsedSeconds = time.Since(started).Seconds()
	result.PeakBackends = stopPeak()
	if err := <-peak; err != nil {
		return err
	}
	summarize(outcomes, result)
	return nil
}

// checkVariant refuses a database whose state does not match the variant, so
// a label cannot misdescribe what was measured.
func checkVariant(ctx context.Context, s *session, variant string) error {
	var query string
	switch variant {
	case "postgres":
		query = `SELECT NOT EXISTS(SELECT 1 FROM archive_file)`
	case "catalog":
		query = `SELECT count(DISTINCT kind)=3 AND coalesce(bool_and(complete),false) FROM archive_lookup_partition`
	case "naive":
		return usageError("--variant naive, which reads every archived file, is not built yet")
	default:
		return usageError("--variant must be postgres or catalog")
	}
	var matches bool
	if err := s.pool.QueryRow(ctx, query).Scan(&matches); err != nil {
		return fmt.Errorf("check the database for --variant %s: %w", variant, err)
	}
	if !matches {
		return fmt.Errorf("the database does not match --variant %s: postgres needs no archived files, and catalog needs complete lookup catalogs", variant)
	}
	return nil
}

type outcome struct {
	key     string
	answer  string
	elapsed time.Duration
	err     error
}

// collect runs samples requests over concurrency workers.
func collect(ctx context.Context, s *session, frozen inputs, read scenario, concurrency, samples int) []outcome {
	jobs := make(chan int)
	outcomes := make([]outcome, samples)
	var workers sync.WaitGroup
	for range concurrency {
		workers.Go(func() {
			for i := range jobs {
				started := time.Now()
				key, answer, err := read(ctx, s, frozen, i)
				outcomes[i] = outcome{key: key, elapsed: time.Since(started), err: err}
				if err == nil {
					outcomes[i].answer = digest(answer)
				}
			}
		})
	}
	for i := range samples {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	return outcomes
}

func digest(answer any) string {
	data, err := json.Marshal(answer)
	if err != nil {
		data = []byte(fmt.Sprintf("unencodable answer: %v", err))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func summarize(outcomes []outcome, result *measurement) {
	var latencies []time.Duration
	answers := map[string]string{}
	for _, o := range outcomes {
		switch {
		case errors.Is(o.err, context.DeadlineExceeded):
			result.Timeouts++
		case o.err != nil:
			result.Errors++
			if result.FirstError == "" {
				result.FirstError = o.err.Error()
			}
		default:
			result.Succeeded++
			latencies = append(latencies, o.elapsed)
			if previous, seen := answers[o.key]; seen && previous != o.answer {
				result.Inconsistent++
			}
			answers[o.key] = o.answer
		}
	}
	slices.Sort(latencies)
	result.P50Millis = percentile(latencies, 0.50)
	result.P95Millis = percentile(latencies, 0.95)
	result.MaxMillis = percentile(latencies, 1)
	lines := make([]string, 0, len(answers))
	for key, answer := range answers {
		lines = append(lines, key+"\t"+answer)
	}
	slices.Sort(lines)
	result.AnswerSHA256 = digest(lines)
}

// percentile returns the nearest-rank percentile in milliseconds, or 0
// without samples.
func percentile(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := max(int(p*float64(len(sorted))+0.999999)-1, 0)
	return float64(sorted[min(rank, len(sorted)-1)].Microseconds()) / 1000
}

// watchBackends samples the database's session count every 100 ms. stop
// ends sampling and returns the peak; the channel reports a sampling error.
func watchBackends(ctx context.Context, s *session) (<-chan error, func() int) {
	ctx, cancel := context.WithCancel(ctx)
	var mu sync.Mutex
	peak := 0
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			var count int
			err := s.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()`).Scan(&count)
			if err != nil && ctx.Err() == nil {
				done <- fmt.Errorf("count database sessions: %w", err)
				return
			}
			mu.Lock()
			peak = max(peak, count)
			mu.Unlock()
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
			}
		}
	}()
	return done, func() int {
		cancel()
		mu.Lock()
		defer mu.Unlock()
		return peak
	}
}
