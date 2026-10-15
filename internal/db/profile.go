package db

// RecentRunsSQL exposes the generated run-summary query for EXPLAIN and timing.
// Unfiltered pages supply the run IDs found by batched discovery.
const RecentRunsSQL = selectRecentRuns

// RecentRunCandidatesSQL exposes one discovery batch for EXPLAIN and timing.
const RecentRunCandidatesSQL = selectRecentRunCandidates
