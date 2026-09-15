import type { createBenchDBClient } from "../api/client";
import type { CompareResult, LookbackAnalysis, PairwiseAnalysis } from "../api/benchdb";
import type { SeriesStatus } from "../browse/transform";
import { resultViewModelFromDetail, type ResultViewModel } from "../result/loader";
import type { ComparisonMarker } from "../series/chart-geometry";
import type { CompareQuery } from "../router";
import { orderSamplesForChart, toSeriesPoints, type SeriesPoint } from "../series/transform";
import { markedIndices, verdictStatus } from "./transform";

type Client = ReturnType<typeof createBenchDBClient>;

/** NotComparableError carries the endpoint's 422 reason (different series,
 * errored result, unit mismatch) so the page can render it inline as product
 * feedback, distinct from a load failure. */
export class NotComparableError extends Error {}

export interface CompareViewModel {
  status: SeriesStatus;
  lookback: LookbackAnalysis;
  pairwise: PairwiseAnalysis;
  unit: string;
  lessIsBetter: boolean;
  baseline: ResultViewModel;
  contender: ResultViewModel;
  points: SeriesPoint[];
  marked: number[];
  markers: ComparisonMarker[];
}

async function fetchSelectedResult(client: Client, id: string, role: ComparisonMarker["role"]) {
  const res = await client.getBenchmarkResult(id);
  if (res.status >= 400 || !res.data) throw new Error(`failed to load benchmark result ${id}`);
  const detail = res.data;
  const chartMs = Date.parse(detail.commit?.timestamp ?? detail.timestamp);
  const marker: ComparisonMarker | null = detail.single_value_summary === null || detail.unit === null || !Number.isFinite(chartMs)
    ? null
    : { role, resultId: id, chartMs, value: detail.single_value_summary, unit: detail.unit };
  return { result: resultViewModelFromDetail(detail), marker };
}

async function fetchVerdicts(client: Client, query: CompareQuery): Promise<CompareResult> {
  const res = await client.compareBenchmarkResults({
        baseline_result_id: query.baseline,
        contender_result_id: query.contender,
        ...(query.threshold !== null ? { threshold: query.threshold } : {}),
        ...(query.thresholdZ !== null ? { threshold_z: query.thresholdZ } : {}),
      });
  if (res.status >= 400 || !res.data) {
    if (res.status === 422) {
      throw new NotComparableError((res.data as { detail?: string })?.detail ?? "results are not comparable");
    }
    throw new Error(`failed to compare ${query.baseline} vs ${query.contender}`);
  }
  return res.data;
}

async function fetchPoints(client: Client, resultId: string, unit: string): Promise<SeriesPoint[]> {
  const res = await client.getHistoryForResult(resultId);
  if (res.status >= 400 || !res.data) {
    throw new Error(`failed to load history for benchmark result ${resultId}`);
  }
  const samples = (res.data.samples ?? []).filter((sample) => sample.unit === unit);
  return toSeriesPoints(orderSamplesForChart(samples));
}

/** loadCompare resolves the verdicts first — the endpoint's 422 throws
 * NotComparableError before anything else loads — then both sides' identities
 * (using the result page's view model) and default-branch history in parallel.
 * Selected markers come from result detail, not history membership, and never
 * enter the rolling statistics. Throws a plain Error for other
 * failures; the page owns presentation. */
export async function loadCompare(client: Client, query: CompareQuery): Promise<CompareViewModel> {
  const verdicts = await fetchVerdicts(client, query);
  const [baseline, contender, points] = await Promise.all([
    fetchSelectedResult(client, query.baseline, "baseline"),
    fetchSelectedResult(client, query.contender, "contender"),
    fetchPoints(client, query.baseline, verdicts.unit),
  ]);
  return {
    status: verdictStatus(verdicts.analysis.lookback_z_score),
    lookback: verdicts.analysis.lookback_z_score,
    pairwise: verdicts.analysis.pairwise,
    unit: verdicts.unit,
    lessIsBetter: verdicts.less_is_better,
    baseline: baseline.result,
    contender: contender.result,
    markers: [baseline.marker, contender.marker].filter((marker): marker is ComparisonMarker => marker !== null),
    points,
    marked: markedIndices(points, [query.baseline, query.contender]),
  };
}
