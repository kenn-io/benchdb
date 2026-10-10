import type { createBenchDBClient } from "../api/client";
import type { RecentRunListItem } from "../api/benchdb";
import { repositoryLabel } from "../repository";

type Client = ReturnType<typeof createBenchDBClient>;
type RecentRun = RecentRunListItem;
type RecentRunCommit = NonNullable<RecentRun["commit"]> & {
  message?: string | null;
  author_name?: string | null;
  author_login?: string | null;
  author_avatar?: string | null;
};

export interface RecentRunViewModel {
  runId: string;
  displayRunId: string;
  primaryLabel: string;
  runHref: string;
  runReason: string | null;
  machineNames: string[];
  machineLabel: string;
  batchCount: number;
  latestBatchId: string | null;
  displayLatestBatchId: string | null;
  latestBatchHref: string | null;
  resultCount: number;
  errorCount: number;
  repository: string;
  repositoryLabel: string;
  shortCommit: string | null;
  commitHref: string | null;
  authorLabel: string;
  authorAvatar: string | null;
  lastResultAt: string;
  attention: RecentRunAttentionViewModel | null;
}

export interface RecentRunAttentionViewModel {
  status: "failure" | "action_required";
  statusReason: string;
  // The server's report link names the exact repository and commit the
  // attention summary was computed for.
  reportHref: string;
  summaryText: string;
}

export interface RecentRunsViewModel {
  hasMore: boolean;
  runs: RecentRunViewModel[];
}

export interface RecentRunsQuery {
  repository: string;
  q?: string;
  offset?: number;
}

export const RECENT_RUNS_PAGE_SIZE = 25;

function recentRunsError(res: { data?: unknown }): Error {
  return new Error((res.data as { detail?: string })?.detail ?? "failed to list recent runs");
}

export async function listRecentRuns(
  client: Client,
  query: RecentRunsQuery = { repository: "" },
): Promise<RecentRunsViewModel> {
  const apiQuery: {
    page_size: number;
    include_attention: boolean;
    repository?: string;
    q?: string;
    offset?: number;
  } = { page_size: RECENT_RUNS_PAGE_SIZE, include_attention: true };
  if (query.repository !== "") {
    apiQuery.repository = query.repository;
  }
  if (query.q) apiQuery.q = query.q;
  if (query.offset) apiQuery.offset = query.offset;
  const res = await client.listRecentRuns(apiQuery);
  if (res.status >= 400 || !res.data) {
    throw recentRunsError(res);
  }
  return {
    hasMore: res.data.has_more,
    runs: (res.data.runs ?? []).map(toRecentRunViewModel),
  };
}

function toRecentRunViewModel(run: RecentRun): RecentRunViewModel {
  const commitSha = run.commit_sha ?? null;
  const shortCommit = commitSha === null ? null : commitSha.slice(0, 8);
  const displayRunId = compactIdentifier(run.run_id, 12, 8);
  const commit = (run.commit ?? null) as RecentRunCommit | null;
  const commitMessage = cleanString(commit?.message ?? null);
  const authorLogin = cleanString(commit?.author_login ?? null);
  const authorName = cleanString(commit?.author_name ?? null);
  return {
    runId: run.run_id,
    displayRunId,
    primaryLabel: commitMessage ?? shortCommit ?? displayRunId,
    runHref: `/runs/${encodeURIComponent(run.run_id)}`,
    runReason: run.run_reason ?? null,
    machineNames: run.machine_names ?? [],
    machineLabel: machineLabel(run.machine_names ?? []),
    batchCount: run.batch_count,
    latestBatchId: run.latest_batch_id ?? null,
    displayLatestBatchId: run.latest_batch_id === null
      ? null
      : compactIdentifier(run.latest_batch_id, 12, 6),
    latestBatchHref: run.latest_batch_id === null ? null : `/batches/${encodeURIComponent(run.latest_batch_id)}`,
    resultCount: run.result_count,
    errorCount: run.error_count,
    repository: run.repository,
    repositoryLabel: repositoryLabel(run.repository),
    shortCommit,
    commitHref: commitHref(run.repository, commitSha),
    authorLabel: authorName ?? authorLogin ?? "unknown author",
    authorAvatar: usableHTTPURL(commit?.author_avatar ?? null),
    lastResultAt: run.last_result_at,
    attention: toRecentRunAttentionViewModel(run.attention ?? null),
  };
}

function machineLabel(names: string[]): string {
  if (names.length === 0) return "machine not reported";
  if (names.length === 1) return names[0]!;
  return `${names.length.toLocaleString()} machines`;
}

function toRecentRunAttentionViewModel(
  attention: NonNullable<RecentRun["attention"]> | null,
): RecentRunAttentionViewModel | null {
  if (attention === null || attention.status === "success" || attention.status === "skipped") {
    return null;
  }
  return {
    status: attention.status,
    statusReason: attention.status_reason,
    reportHref: attention.report_url,
    summaryText: attentionSummaryText(attention.summary),
  };
}

function attentionSummaryText(summary: NonNullable<RecentRun["attention"]>["summary"]): string {
  if (summary.regressions > 0) {
    return plural(summary.regressions, "regression");
  }
  if (summary.benchmark_errors > 0) {
    return plural(summary.benchmark_errors, "benchmark error");
  }
  if (summary.missing_baseline > 0) {
    return plural(summary.missing_baseline, "missing baseline", "missing baselines");
  }
  if (summary.not_comparable > 0) {
    return plural(summary.not_comparable, "not comparable row");
  }
  return "action required";
}

function plural(n: number, word: string, pluralWord = `${word}s`): string {
  return `${n.toLocaleString()} ${n === 1 ? word : pluralWord}`;
}

function compactIdentifier(value: string, head: number, tail: number): string {
  if (value.length <= head + tail + 1) {
    return value;
  }
  return `${value.slice(0, head)}…${value.slice(-tail)}`;
}

function commitHref(repository: string, commitSha: string | null): string | null {
  if (commitSha === null || commitSha === "") {
    return null;
  }
  let u: URL;
  try {
    u = new URL(repository);
  } catch {
    return null;
  }
  if (u.hostname !== "github.com" && u.hostname !== "www.github.com") {
    return null;
  }
  const parts = u.pathname.split("/").filter(Boolean);
  if (parts.length < 2) {
    return null;
  }
  return `https://github.com/${parts[0]}/${parts[1]}/commit/${encodeURIComponent(commitSha)}`;
}

function cleanString(value: string | null): string | null {
  const trimmed = value?.trim() ?? "";
  return trimmed === "" ? null : trimmed;
}

function usableHTTPURL(value: string | null): string | null {
  const cleaned = cleanString(value);
  if (cleaned === null) {
    return null;
  }
  let u: URL;
  try {
    u = new URL(cleaned);
  } catch {
    return null;
  }
  return u.protocol === "https:" || u.protocol === "http:" ? cleaned : null;
}
