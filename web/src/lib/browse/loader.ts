import type { createBenchDBClient } from "../api/client";
import type { BrowseQuery } from "../router";
import { toBrowseRows, windowStartIso, type BrowseRow, type SeriesStatus } from "./transform";

type Client = ReturnType<typeof createBenchDBClient>;

/** BrowseStatusCounts counts every benchmark the other filters match, by status. */
export type BrowseStatusCounts = Record<SeriesStatus, number>;

export interface BrowsePage {
  rows: BrowseRow[];
  statusCounts: BrowseStatusCounts;
  /** A queued recompute can still change benchmarks in the project filter. */
  statusesPending: boolean;
  nextCursor: string | null;
}

export const BROWSE_PAGE_SIZE = 200;

function listBenchmarksError(res: { data?: unknown }): Error {
  return new Error((res.data as { detail?: string })?.detail ?? "failed to list benchmarks");
}

/** listSeries fetches one page of logical benchmarks for the browse table. Filters
 * map 1:1 onto the endpoint; the window preset becomes an absolute UTC
 * active_since at call time. Throws on any error so the page's error state owns
 * presentation. now is injectable for tests. */
export async function listSeries(
  client: Client,
  query: BrowseQuery,
  cursor: string | null = null,
  now: Date = new Date(),
): Promise<BrowsePage> {
  const since = windowStartIso(query.window, now);
  const res = await client.listBenchmarks({
        page_size: BROWSE_PAGE_SIZE,
        ...(query.q !== "" && { q: query.q }),
        ...(query.hardware !== "" && { hardware: query.hardware }),
        ...(query.repository !== "" && { repository: query.repository }),
        ...(since !== null && { active_since: since }),
        ...(query.status !== "" && { status: query.status }),
        ...(cursor !== null && { cursor }),
      });
  if (res.status >= 400 || !res.data) {
    throw listBenchmarksError(res);
  }
  // The generated schema types `benchmarks` as nullable (a Go nil slice serializes
  // as JSON null); treat null as an empty page, matching the history loader.
  return {
    rows: toBrowseRows(res.data.benchmarks ?? []),
    statusCounts: res.data.status_counts,
    statusesPending: res.data.statuses_pending,
    nextCursor: res.data.next_page_cursor,
  };
}
