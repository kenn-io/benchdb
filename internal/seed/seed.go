// Package seed populates a development database with a deterministic, hand-built
// benchmark history so `make dev` produces a useful demo: one logical
// benchmark measured on two machines across several default-branch commits,
// with ordinary noise, a regression, and a recovery. Two excluded results (an
// off-branch commit and an errored run) exercise the membership rules.
//
// It seeds through the ingestion service (not raw SQL) so the seeded rows are
// produced by exactly the same path as real submissions. It is idempotent: it
// skips when the database already holds results.
package seed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/benchdb/internal/auth"
	"go.kenn.io/benchdb/internal/commit"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

const (
	repo          = "https://github.com/benchdb/demo"
	benchmarkName = "ingest-events-10m"

	// IncludedHistoryPoints is how many seeded results are members of the
	// history series (the default-branch, non-errored commits).
	IncludedHistoryPoints = 15
)

// Summary reports what a seed run did.
type Summary struct {
	Fingerprint  string              // the seeded history series (empty when skipped)
	ProductSmoke ProductSmokeTargets // named targets for API/browser/docs smoke coverage
	Inserted     int                 // results submitted
	Skipped      bool                // true when the database already held results
}

// ProductSmokeTargets names deterministic seed rows used by product smoke tests,
// docs screenshots, and migration validation. The IDs come from normal ingestion;
// the run, batch, repository, and commit values are stable seed inputs.
type ProductSmokeTargets struct {
	Repository                string
	Fingerprint               string
	LatestResultID            string
	BaselineResultID          string
	ContenderResultID         string
	RecentRunID               string
	RecentBatchID             string
	CIRegressionRunID         string
	CIRegressionCommitSHA     string
	CIRegressionRunReason     string
	CIActionRequiredRunID     string
	CIActionRequiredCommitSHA string
	ErroredRunID              string
	ErroredResultID           string
}

// includedCommits are the default-branch commits whose results form the history
// series. The minima trend upward (a slowdown, with a regression at commit-04).
var includedCommits = []struct {
	sha        string
	message    string
	day        int
	armMinimum float64
	x86Minimum float64
}{
	{"commit-01", "Establish streaming baseline", 0, 82.0, 112.0},
	{"commit-02", "Reuse decode buffers", 2, 81.2, 110.5},
	{"commit-03", "Vectorize timestamp parsing", 4, 80.5, 109.8},
	{"commit-04", "Batch attribute decoding", 7, 79.8, 108.2},
	{"commit-05", "Avoid intermediate maps", 9, 78.9, 107.6},
	{"commit-06", "Add pooled key storage", 12, 78.4, 106.8},
	{"commit-07", "Parallelize block decoding", 15, 76.8, 103.9},
	{"commit-08", "Reduce allocator pressure", 18, 75.9, 102.8},
	{"commit-09", "Inline the record hot path", 21, 74.8, 101.5},
	{"commit-10", "Add schema projection", 24, 74.1, 100.9},
	{"commit-11", "Refactor the record builder", 27, 75.2, 102.4},
	{"commit-12", "Validate nested fields", 31, 88.6, 119.3},
	{"commit-13", "Cache the compiled schema", 35, 84.0, 113.1},
	{"commit-14", "Skip unused fields", 39, 79.7, 107.2},
	{"commit-15", "Restore streaming decode", 42, 76.2, 102.5},
}

type demoMachine struct {
	label        string
	name         string
	architecture string
	cpu          string
	cores        int32
	memoryBytes  int64
}

var demoMachines = []demoMachine{
	{label: "arm64", name: "runner-arm64", architecture: "arm64", cpu: "Neoverse V2", cores: 16, memoryBytes: 64 << 30},
	{label: "x86-64", name: "runner-x86-64", architecture: "x86_64", cpu: "EPYC 9454P", cores: 24, memoryBytes: 128 << 30},
}

// Run seeds the database, returning a summary. It is idempotent: when results
// already exist it makes no changes.
func Run(ctx context.Context, store storage.Store) (Summary, error) {
	n, err := store.CountBenchmarkResults(ctx)
	if err != nil {
		return Summary{}, fmt.Errorf("count results: %w", err)
	}
	if n > 0 {
		return Summary{Skipped: true}, nil
	}

	ingester := service.NewIngester(store, commit.MapProvider{Commits: commitMap()})
	reqs := requests()
	var fingerprint string
	resultByLabel := make(map[string]*service.Result, len(reqs))
	for _, r := range reqs {
		res, err := ingester.Submit(ctx, r.req)
		if err != nil {
			return Summary{}, fmt.Errorf("seed submit %s: %w", r.label, err)
		}
		resultByLabel[r.label] = res
		if r.included && r.label == "commit-15-arm64" {
			fingerprint = res.HistoryFingerprint
		}
	}
	targets, err := productSmokeTargets(fingerprint, resultByLabel)
	if err != nil {
		return Summary{}, err
	}
	return Summary{Fingerprint: fingerprint, ProductSmoke: targets, Inserted: len(reqs)}, nil
}

// DevTokenStore is the slice of the data layer DevToken needs. *db.Store
// satisfies it.
type DevTokenStore interface {
	GetOrCreateUserByEmail(ctx context.Context, email, name, password string) (string, error)
	GetAPITokenByHash(ctx context.Context, tokenHash string) (storage.APIToken, error)
	CreateAPIToken(ctx context.Context, p storage.InsertAPITokenParams) (string, error)
}

// DevToken get-or-creates a dev user and a user-attributed API token whose hash
// is auth.HashToken(plaintext), so a development or e2e run can exercise
// user-attributed endpoints without an OIDC round-trip. It is idempotent: if a
// token with that hash already exists it does nothing. The plaintext is never
// logged (only the prefix is returned). It requires plaintext of at least 8
// characters (the prefix width).
func DevToken(ctx context.Context, store DevTokenStore, plaintext string) (prefix string, err error) {
	runes := []rune(plaintext)
	if len(runes) < 8 {
		return "", fmt.Errorf("dev token must be at least 8 characters, got %d", len(runes))
	}
	prefix = string(runes[:8]) // rune-safe so a multi-byte value cannot cut mid-rune
	hash := auth.HashToken(plaintext)

	_, err = store.GetAPITokenByHash(ctx, hash)
	switch {
	case err == nil:
		return prefix, nil
	case errors.Is(err, storage.ErrNotFound):
		// fall through to create
	default:
		return "", fmt.Errorf("lookup dev token: %w", err)
	}

	userID, err := store.GetOrCreateUserByEmail(ctx, "dev@benchdb.local", "Dev (seeded)", "!")
	if err != nil {
		return "", fmt.Errorf("get-or-create dev user: %w", err)
	}
	if _, err := store.CreateAPIToken(ctx, storage.InsertAPITokenParams{
		UserID:      userID,
		Name:        "dev seed token",
		TokenHash:   hash,
		TokenPrefix: prefix,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		return "", fmt.Errorf("create dev token: %w", err)
	}
	return prefix, nil
}

// seedResult is one planned submission and whether it should join the history.
type seedResult struct {
	label    string
	included bool
	req      service.SubmitRequest
}

// requests is the full seed plan: the included series plus two excluded results.
func requests() []seedResult {
	out := make([]seedResult, 0, len(includedCommits)*len(demoMachines)+2)
	for _, c := range includedCommits {
		for _, machine := range demoMachines {
			minimum := c.armMinimum
			if machine.label == "x86-64" {
				minimum = c.x86Minimum
			}
			label := c.sha + "-" + machine.label
			out = append(out, seedResult{
				label:    label,
				included: true,
				req:      baseReq(c.sha, c.day, trend(minimum), machine),
			})
		}
	}
	// Excluded: an off-branch commit (sha != fork_point_sha), shaped as a clear
	// PR regression against commit-03 for CI report and alert smoke coverage.
	featureReq := baseReq("feature-branch-1", 6, trend(108.0), demoMachines[0])
	featureReq.RunReason = "pull request"
	out = append(out, seedResult{label: "feature-branch-1", included: false, req: featureReq})
	// Excluded: an errored run (a missing iteration -> partial result).
	out = append(out, seedResult{
		label:    "commit-16-broken",
		included: false,
		req:      baseReq("commit-16-broken", 45, partial(76.0), demoMachines[0]),
	})
	return out
}

func productSmokeTargets(fingerprint string, results map[string]*service.Result) (ProductSmokeTargets, error) {
	resultID := func(label string) (string, error) {
		res, ok := results[label]
		if !ok || res == nil || res.ID == "" {
			return "", fmt.Errorf("seed target %s missing result id", label)
		}
		return res.ID, nil
	}

	latestID, err := resultID("commit-15-arm64")
	if err != nil {
		return ProductSmokeTargets{}, err
	}
	baselineID, err := resultID("commit-01-arm64")
	if err != nil {
		return ProductSmokeTargets{}, err
	}
	contenderID, err := resultID("commit-12-arm64")
	if err != nil {
		return ProductSmokeTargets{}, err
	}
	erroredID, err := resultID("commit-16-broken")
	if err != nil {
		return ProductSmokeTargets{}, err
	}

	return ProductSmokeTargets{
		Repository:                repo,
		Fingerprint:               fingerprint,
		LatestResultID:            latestID,
		BaselineResultID:          baselineID,
		ContenderResultID:         contenderID,
		RecentRunID:               "run-commit-15",
		RecentBatchID:             "batch-commit-15",
		CIRegressionRunID:         "run-feature-branch-1",
		CIRegressionCommitSHA:     "feature-branch-1",
		CIRegressionRunReason:     "pull request",
		CIActionRequiredRunID:     "run-commit-05",
		CIActionRequiredCommitSHA: "commit-05",
		ErroredRunID:              "run-commit-16-broken",
		ErroredResultID:           erroredID,
	}, nil
}

// commitMap gives each seeded sha a real message and timestamp. Included commits
// default to the default branch (fork_point_sha == sha); feature-branch-1 forks
// from commit-03 so it is excluded from history.
func commitMap() map[string]commit.Info {
	m := make(map[string]commit.Info, len(includedCommits)+2)
	for _, c := range includedCommits {
		ts := day(c.day)
		m[c.sha] = commit.Info{Message: c.message, Timestamp: &ts}
	}
	branchPoint := includedCommits[2].sha
	featureTS := day(4)
	m["feature-branch-1"] = commit.Info{Message: "WIP: experimental layout", Timestamp: &featureTS, ForkPointSha: &branchPoint}
	brokenTS := day(45)
	m["commit-16-broken"] = commit.Info{Message: "Broken nightly run", Timestamp: &brokenTS}
	return m
}

// baseReq builds a submission sharing one fingerprint (case/context/hardware/repo)
// for the given commit, timestamp, and samples.
func baseReq(sha string, d int, data []*float64, machine demoMachine) service.SubmitRequest {
	runName := "nightly"
	runSuffix := ""
	if machine.label != "arm64" {
		runSuffix = "-" + machine.label
	}
	return service.SubmitRequest{
		Tags:        map[string]any{"name": benchmarkName, "dataset": "10m-events", "metric": "wall-time"},
		Context:     map[string]any{"runtime": "go1.27", "workers": 8},
		Info:        map[string]any{"benchmark_language": "Go"},
		MachineInfo: machineInfo(machine),
		GitHub:      service.GitHubInfo{Commit: sha, Repository: repo},
		RunID:       "run-" + sha + runSuffix,
		RunName:     &runName,
		BatchID:     "batch-" + sha + runSuffix,
		Timestamp:   day(d),
		Stats:       &service.StatsInput{Data: data, Unit: "s"},
	}
}

func machineInfo(machine demoMachine) *service.MachineInfo {
	architectureName := machine.architecture
	osName := "Linux"
	cpuModelName := machine.cpu
	cpuCoreCount := machine.cores
	cpuThreadCount := machine.cores
	memoryBytes := machine.memoryBytes
	gpuCount := int32(0)
	return &service.MachineInfo{
		Name:             machine.name,
		ArchitectureName: &architectureName,
		OsName:           &osName,
		CpuModelName:     &cpuModelName,
		CpuCoreCount:     &cpuCoreCount,
		CpuThreadCount:   &cpuThreadCount,
		MemoryBytes:      &memoryBytes,
		GpuCount:         &gpuCount,
	}
}

// trend returns three increasing samples whose minimum (the single value summary
// for the less-is-better "s" unit) is min.
func trend(min float64) []*float64 {
	a, b, c := min, min+0.02, min+0.04
	return []*float64{&a, &b, &c}
}

// partial returns samples with a missing middle iteration, marking an errored run.
func partial(min float64) []*float64 {
	a, c := min, min+0.04
	return []*float64{&a, nil, &c}
}

func day(d int) time.Time {
	return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC).AddDate(0, 0, d)
}
