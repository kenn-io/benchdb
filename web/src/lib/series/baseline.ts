import type { createBenchDBClient } from "../api/client";

type Client = ReturnType<typeof createBenchDBClient>;

/** saveSelectedBaseline asks the server to average the series' results from
 * startResultId through throughResultId and fix the baseline there. It
 * resolves to an error message, or null once saved. */
export async function saveSelectedBaseline(
  client: Client,
  startResultId: string,
  throughResultId: string,
): Promise<string | null> {
  const res = await client.setResultBaselineOverride(startResultId, { through_result_id: throughResultId });
  if (res.status < 400) return null;
  const detail = (res.data as unknown as { detail?: unknown } | null)?.detail;
  if (res.status === 401) return "Sign in to save a baseline.";
  return typeof detail === "string" ? detail : `Saving the baseline failed (HTTP ${res.status}).`;
}
