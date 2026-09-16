import type { createBenchDBClient } from "../api/client";
import type { CIReport, GetCiReportParams } from "../api/benchdb";
export type { CIReport } from "../api/benchdb";
import type { CIReportQuery } from "../router";

type Client = ReturnType<typeof createBenchDBClient>;

type Query = GetCiReportParams;

function apiQuery(query: CIReportQuery): Query {
  const out: Query = {};
  if (query.repository !== "") out.repository = query.repository;
  if (query.commit !== "") out.commit_sha = query.commit;
  if (query.runIDs !== "") out.run_ids = query.runIDs;
  if (query.baselineRunIDs !== "") out.baseline_run_ids = query.baselineRunIDs;
  if (query.baseline !== "") out.baseline = query.baseline;
  if (query.threshold !== "") out.threshold = Number(query.threshold);
  if (query.thresholdZ !== "") out.threshold_z = Number(query.thresholdZ);
  return out;
}

export function hasCIReportSelector(query: CIReportQuery): boolean {
  return (query.repository !== "" && query.commit !== "") || query.runIDs !== "";
}

export async function loadCIReport(client: Client, query: CIReportQuery): Promise<CIReport> {
  const res = await client.getCiReport(apiQuery(query));
  if (res.status >= 400 || !res.data) {
    throw new Error((res.data as { detail?: string })?.detail ?? "failed to load CI report");
  }
  return res.data;
}
