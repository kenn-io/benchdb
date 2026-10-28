package columnar

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// errRemoteLookupsIncomplete rejects reads that would select remote facts
// before the lookup backfill has completed the directory.
var errRemoteLookupsIncomplete = errors.New("the archive lookup directory is incomplete; remote archive reads wait for the lookup backfill to finish")

// queryScope names the archived facts one query may read. Every fact query
// declares its scope before SQL runs, and only the scope's candidate files are
// opened. Hot rows and the mutation overlay are always visible. Candidates are
// a superset: deleted or ineligible members can still select a file, so
// queries keep their own predicates. An empty scope selects no archive file.
type queryScope struct {
	ResultIDs    []string
	RunIDs       []string
	Fingerprints []string
	CommitIDs    []string
	// AllFacts selects every archived file, for requests with no selective
	// filter. It is never implied by an empty scope.
	AllFacts bool
}

// selectFacts replaces the snapshot's benchmark_result view with hot rows and
// the scope's archived candidates, resolved in the same snapshot.
func (s *ReadSnapshot) selectFacts(ctx context.Context, scope queryScope) error {
	files, err := s.factFiles(ctx, scope)
	if err != nil {
		return err
	}
	source, err := s.engine.logicalSource(files, true)
	if err != nil {
		return err
	}
	_, err = s.tx.ExecContext(ctx, `CREATE OR REPLACE TEMP VIEW benchmark_result AS `+source)
	return err
}

// archivedFacts returns a source over only the scope's archived candidates,
// with the overlay applied and no hot rows, for queries that combine sources.
// It returns no source when the scope selects no file.
func (s *ReadSnapshot) archivedFacts(ctx context.Context, scope queryScope) (string, error) {
	files, err := s.factFiles(ctx, scope)
	if err != nil || len(files) == 0 {
		return "", err
	}
	return s.engine.logicalSource(files, false)
}

// factFiles resolves the scope to pinned manifest files, sorted by path.
func (s *ReadSnapshot) factFiles(ctx context.Context, scope queryScope) ([]archiveFile, error) {
	var files []archiveFile
	var err error
	switch {
	case scope.AllFacts:
		files = s.files
	case s.lookups:
		files, err = s.lookupCandidates(ctx, scope)
	case s.engine.location.remote() && len(s.files) > 0:
		// Selecting without lookups can open every remote fact.
		err = errRemoteLookupsIncomplete
	default:
		files, err = s.summaryCandidates(ctx, scope)
	}
	if err != nil {
		return nil, fmt.Errorf("select archive facts: %w", err)
	}
	if s.engine.observeFacts != nil {
		paths := make([]string, len(files))
		for i, file := range files {
			paths[i] = file.Path
		}
		s.engine.observeFacts(paths)
	}
	return files, nil
}

// lookupCandidates unions the lookup catalogs' files for result, run, and
// fingerprint keys with the commit membership's files. A missing or corrupt
// lookup is an error, never a reason to read every fact.
func (s *ReadSnapshot) lookupCandidates(ctx context.Context, scope queryScope) ([]archiveFile, error) {
	paths := map[string]bool{}
	for kind, keys := range [...][]string{lookupResult: scope.ResultIDs, lookupRun: scope.RunIDs, lookupFingerprint: scope.Fingerprints} {
		files, err := s.engine.lookupFiles(ctx, s, lookupKind(kind), keys)
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			paths[file.Path] = true
		}
	}
	if err := s.addCommitFiles(ctx, paths, scope.CommitIDs); err != nil {
		return nil, err
	}
	return s.manifestFiles(paths)
}

// summaryCandidates selects facts before the lookup directory is complete.
// Run and fingerprint keys use the per-file summary catalogs, which cover
// every manifest file and keep ineligible history groups as membership;
// result IDs have no other index, so they select every file. Lookups are not
// consulted until the backfill completes them.
func (s *ReadSnapshot) summaryCandidates(ctx context.Context, scope queryScope) ([]archiveFile, error) {
	if len(scope.ResultIDs) > 0 {
		return s.files, nil
	}
	if len(scope.RunIDs) > 0 {
		if err := s.runSummaries(ctx); err != nil {
			return nil, err
		}
	}
	if len(scope.Fingerprints) > 0 {
		if err := s.seriesSummaries(ctx); err != nil {
			return nil, err
		}
	}
	paths := map[string]bool{}
	for _, summary := range []struct {
		query string
		keys  []string
	}{
		{`SELECT DISTINCT archive_path FROM archive_run_catalog WHERE run_id IN (SELECT unnest($1::VARCHAR[]))`, scope.RunIDs},
		{`SELECT DISTINCT archive_path FROM archive_series_catalog WHERE history_fingerprint IN (SELECT unnest($1::VARCHAR[]))`, scope.Fingerprints},
	} {
		if len(summary.keys) == 0 {
			continue
		}
		selected, err := collectRows(ctx, s, summary.query, []any{summary.keys}, scanString)
		if err != nil {
			return nil, err
		}
		for _, path := range selected {
			paths[path] = true
		}
	}
	if err := s.addCommitFiles(ctx, paths, scope.CommitIDs); err != nil {
		return nil, err
	}
	return s.manifestFiles(paths)
}

// addCommitFiles adds the files whose captured commit membership includes
// any of commits, through archive_file_commit's commit_id index.
func (s *ReadSnapshot) addCommitFiles(ctx context.Context, paths map[string]bool, commits []string) error {
	if len(commits) == 0 {
		return nil
	}
	values := make([]string, len(commits))
	for i, commit := range commits {
		values[i] = literal(commit)
	}
	query := `SELECT DISTINCT archive_path FROM archive_file_commit WHERE commit_id=ANY(ARRAY[` + strings.Join(values, ",") + `]::text[])`
	selected, err := collectRows(ctx, s, `SELECT archive_path FROM postgres_query('pg',`+literal(query)+`)`, nil, scanString)
	if err != nil {
		return fmt.Errorf("read archived commit membership: %w", err)
	}
	for _, path := range selected {
		paths[path] = true
	}
	return nil
}

// manifestFiles maps selected paths to the pinned manifest, in path order.
// Every index is read in the manifest's snapshot, so a path outside it is an
// inconsistency.
func (s *ReadSnapshot) manifestFiles(paths map[string]bool) ([]archiveFile, error) {
	files := make([]archiveFile, 0, len(paths))
	for _, file := range s.files {
		if paths[file.Path] {
			files = append(files, file)
		}
	}
	if len(files) != len(paths) {
		return nil, errors.New("an archive index selected a file outside the pinned manifest")
	}
	return files, nil
}
