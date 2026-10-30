package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/spf13/pflag"

	"go.kenn.io/benchdb/internal/storage"
)

// inputs are the frozen keys every variant measures.
type inputs struct {
	Dataset      string   `json:"dataset"`
	Runs         []string `json:"runs"`
	Results      []string `json:"results"`
	Fingerprints []string `json:"fingerprints"`
}

func runInputs(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := pflag.NewFlagSet("archive-perf inputs", pflag.ContinueOnError)
	flags.SetOutput(stderr)
	var t target
	t.register(flags)
	runs := flags.Int("runs", 20, "runs to sample")
	output := flags.String("output", "", "file for the inputs JSON; stdout when empty")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := t.check(); err != nil {
		return err
	}
	if *runs <= 0 {
		return usageError("--runs must be positive")
	}
	s, err := t.open(ctx)
	if err != nil {
		return err
	}
	frozen, sampleErr := sampleInputs(ctx, s, *runs)
	if err := errors.Join(sampleErr, s.close()); err != nil {
		return err
	}
	frozen.Dataset = t.expectDataset
	return writeJSON(stdout, *output, frozen)
}

// sampleInputs picks runs in a stable pseudo-random order, so the same
// database yields the same inputs, and derives one result and its history
// from each run through the store.
func sampleInputs(ctx context.Context, s *session, limit int) (inputs, error) {
	rows, err := s.pool.Query(ctx, `SELECT run_id FROM (SELECT DISTINCT run_id FROM run_membership) runs ORDER BY md5(run_id), run_id LIMIT $1`, limit)
	if err != nil {
		return inputs{}, fmt.Errorf("sample runs: %w", err)
	}
	var runs []string
	for rows.Next() {
		var run string
		if err := rows.Scan(&run); err != nil {
			rows.Close()
			return inputs{}, fmt.Errorf("sample runs: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return inputs{}, fmt.Errorf("sample runs: %w", err)
	}
	frozen := inputs{Runs: runs}
	for _, run := range runs {
		ids, err := s.store.SelectRunResultIDs(ctx, run)
		if err != nil {
			return inputs{}, fmt.Errorf("read the results of run %s: %w", run, err)
		}
		if len(ids) == 0 {
			continue
		}
		slices.Sort(ids)
		detail, err := s.store.GetBenchmarkResultDetail(ctx, ids[0])
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return inputs{}, fmt.Errorf("read result %s: %w", ids[0], err)
		}
		frozen.Results = append(frozen.Results, ids[0])
		frozen.Fingerprints = append(frozen.Fingerprints, detail.HistoryFingerprint)
	}
	if len(frozen.Results) == 0 {
		return inputs{}, errors.New("the database has no runs with readable results")
	}
	return frozen, nil
}

func writeJSON(stdout io.Writer, path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if path == "" {
		_, err = stdout.Write(data)
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func readInputs(path, dataset string) (inputs, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return inputs{}, fmt.Errorf("read --inputs: %w", err)
	}
	var frozen inputs
	if err := json.Unmarshal(data, &frozen); err != nil {
		return inputs{}, fmt.Errorf("decode --inputs: %w", err)
	}
	if frozen.Dataset != dataset {
		return inputs{}, fmt.Errorf("the inputs were sampled from dataset %s, not %s", frozen.Dataset, dataset)
	}
	return frozen, nil
}
