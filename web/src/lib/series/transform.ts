import {
  localDateStr,
  resolveRange,
  type RangeSelection,
} from "@kenn-io/kit-ui";

import type { HistorySample, ZScoreStats } from "../api/benchdb";
import { formatDate } from "../browse/transform";
import { exactMeasurement, formatMeasurement } from "../format";

/** PointStats is the per-point display math derived from the engine's
 * zscorestats. The client only divides and scales — segment/step detection and
 * regression verdicts are the frozen server engine's job. */
export interface PointStats {
  z: number | null;
  rollingMean: number | null;
  rollingStddev: number | null;
  isOutlier: boolean;
  isStep: boolean;
  beginsChange: boolean;
  /** fixedBaseline: the means are a selected baseline, not a rolling window. */
  fixedBaseline: boolean;
  segmentId: number | null;
}

export interface SeriesPoint {
  resultId: string;
  commitHash: string;
  commitMessage: string;
  commitTimestampMs: number | null;
  resultTimestampMs: number;
  /** chartMs is the plotted instant: commit time, else result (run) time. */
  chartMs: number;
  measurements: number[];
  svs: number;
  unit: string | null;
  runTags: Record<string, unknown>;
  info: Record<string, unknown>;
  changeAnnotations: Record<string, unknown>;
  stats: PointStats;
}

function toMs(value: string | null): number | null {
  return value === null ? null : Date.parse(value);
}

function toPointStats(zsRaw: ZScoreStats): PointStats {
  if (zsRaw === null) {
    return {
      z: null,
      rollingMean: null,
      rollingStddev: null,
      isOutlier: false,
      isStep: false,
      beginsChange: false,
      fixedBaseline: false,
      segmentId: null,
    };
  }
  const z =
    zsRaw.residual !== null && zsRaw.rolling_stddev !== null && zsRaw.rolling_stddev !== 0
      ? zsRaw.residual / zsRaw.rolling_stddev
      : null;
  return {
    z,
    rollingMean: zsRaw.rolling_mean,
    rollingStddev: zsRaw.rolling_stddev,
    isOutlier: zsRaw.is_outlier,
    isStep: zsRaw.is_step,
    beginsChange: zsRaw.begins_distribution_change,
    fixedBaseline: zsRaw.fixed_baseline,
    segmentId: zsRaw.segment_id,
  };
}

function sampleMeasurements(sample: HistorySample): number[] {
  const data = (sample as HistorySample & { data?: number[] | null }).data;
  return Array.isArray(data) ? data.filter(Number.isFinite) : [];
}

export function toSeriesPoints(samples: HistorySample[]): SeriesPoint[] {
  return samples.map((s) => {
    const stats = toPointStats(s.zscorestats);
    stats.beginsChange ||= s.change_annotations["begins_distribution_change"] === true;
    return {
      resultId: s.benchmark_result_id,
      commitHash: s.commit_hash,
      commitMessage: s.commit_message,
      commitTimestampMs: toMs(s.commit_timestamp),
      resultTimestampMs: Date.parse(s.result_timestamp),
      chartMs: Date.parse(s.commit_timestamp ?? s.result_timestamp),
      measurements: sampleMeasurements(s),
      svs: s.single_value_summary,
      unit: s.unit,
      runTags: s.run_tags,
      info: s.info,
      changeAnnotations: s.change_annotations,
      stats,
    };
  });
}

export interface TableRow {
  index: number;
  resultId: string;
  commitHash: string;
  commitMessage: string;
  chartMs: number;
  svs: number;
  unit: string | null;
  deviation: string | null;
  flags: string;
  machineName?: string;
}

/** flagsText renders the engine's point flags for compact display. "step"
 * covers is_step and begins_distribution_change so the table and tooltip
 * flags match the chart's step markers (stepIndices uses the same rule). */
export function flagsText(stats: PointStats): string {
  const flags: string[] = [];
  if (stats.isStep || stats.beginsChange) flags.push("step");
  if (stats.isOutlier) flags.push("outlier");
  return flags.join(" · ");
}

/** toTableRow describes one point for the results table. index is the
 * point's position in the array the chart renders, so selecting a row selects
 * the same point on the chart even after range filtering. */
export function toTableRow(
  point: SeriesPoint,
  index: number,
  lessIsBetter: boolean | null,
  machineName?: string,
): TableRow {
  return {
    index,
    resultId: point.resultId,
    commitHash: point.commitHash,
    commitMessage: point.commitMessage,
    chartMs: point.chartMs,
    svs: point.svs,
    unit: point.unit,
    deviation: deviationText(point.stats.z, lessIsBetter),
    flags: flagsText(point.stats),
    ...(machineName === undefined ? {} : { machineName }),
  };
}

export function chartTimeExtent(points: SeriesPoint[]): { min: number; max: number } | null {
  if (points.length === 0) return null;
  let min = points[0]!.chartMs;
  let max = min;
  for (let i = 1; i < points.length; i++) {
    min = Math.min(min, points[i]!.chartMs);
    max = Math.max(max, points[i]!.chartMs);
  }
  return { min, max };
}

/** windowAnchorDate anchors a trend range at the newest plotted point,
 * falling back to caller-provided now only for empty data. Trend pages are often
 * opened on historical production series; anchoring to wall-clock today makes
 * those pages appear empty even when the series has useful history. */
export function windowAnchorDate(points: SeriesPoint[], now: Date): Date {
  const latest = chartTimeExtent(points)?.max ?? Number.NEGATIVE_INFINITY;
  return Number.isFinite(latest) ? new Date(latest) : now;
}

/** windowPoints filters the chart's time axis by local calendar date. Relative
 * windows end at the newest plotted point rather than wall-clock today so
 * historical series remain useful. Calendar and custom selections use Kit
 * UI's inclusive date-range contract. */
export function windowPoints(
  points: SeriesPoint[],
  selection: RangeSelection,
  anchor: Date,
): SeriesPoint[] {
  if (selection.mode === "relative" && selection.days <= 0) {
    return points;
  }
  const range =
    selection.mode === "relative"
      ? relativeRange(selection.days, anchor)
      : resolveRange(selection);
  if (range.from === "" || range.to === "") return [];
  return points.filter((point) => {
    const date = localDateStr(new Date(point.chartMs));
    return date >= range.from && date <= range.to;
  });
}

function relativeRange(days: number, anchor: Date): { from: string; to: string } {
  const start = new Date(anchor.getFullYear(), anchor.getMonth(), anchor.getDate());
  start.setDate(start.getDate() - (days - 1));
  return { from: localDateStr(start), to: localDateStr(anchor) };
}

export type TrendChartData = [
  number[],
  (number | null)[],
  (number | null)[],
  (number | null)[],
  (number | null)[],
];

/** trendChartData builds the uPlot-aligned rows [x, svs, mean, hi, lo]. The x
 * values are Unix seconds so horizontal distance always represents elapsed
 * calendar time. The hi/lo rows are rolling_mean ± sigma · rolling_stddev with
 * null gaps wherever the engine has no stats (series start, outliers, fresh
 * segments) — the band re-centers per segment because the underlying rolling
 * stats do. */
export function trendChartData(
  points: SeriesPoint[],
  sigma: number,
): TrendChartData {
  const xs = points.map((p) => p.chartMs / 1000);
  const svs = points.map((p): number | null => p.svs);
  const mean = points.map((p) => p.stats.rollingMean);
  const hi = points.map((p) =>
    p.stats.rollingMean !== null && p.stats.rollingStddev !== null
      ? p.stats.rollingMean + sigma * p.stats.rollingStddev
      : null,
  );
  const lo = points.map((p) =>
    p.stats.rollingMean !== null && p.stats.rollingStddev !== null
      ? p.stats.rollingMean - sigma * p.stats.rollingStddev
      : null,
  );
  return [xs, svs, mean, hi, lo];
}

export function trendYRangeValues(points: SeriesPoint[], sigma: number): number[] {
  return points.flatMap((p) => {
    const values = [p.svs, ...p.measurements];
    if (p.stats.rollingMean !== null) {
      values.push(p.stats.rollingMean);
      if (p.stats.rollingStddev !== null) {
        values.push(p.stats.rollingMean + sigma * p.stats.rollingStddev);
        values.push(p.stats.rollingMean - sigma * p.stats.rollingStddev);
      }
    }
    return values;
  });
}

export function outlierIndices(points: SeriesPoint[]): number[] {
  return points.flatMap((p, i) => (p.stats.isOutlier ? [i] : []));
}

/** stepIndices marks distribution-change boundaries on the chart: points the
 * engine flags is_step or begins_distribution_change (the spec treats both as
 * trend step markers). */
export function stepIndices(points: SeriesPoint[]): number[] {
  return points.flatMap((p, i) => (p.stats.isStep || p.stats.beginsChange ? [i] : []));
}

export interface SegmentSpan {
  startIndex: number;
  endIndex: number;
  segmentId: number;
}

/** segmentSpans groups contiguous runs of the same segment_id for shading;
 * stat-less points (null zscorestats) break a run. */
export function segmentSpans(points: SeriesPoint[]): SegmentSpan[] {
  const spans: SegmentSpan[] = [];
  for (let i = 0; i < points.length; i++) {
    const id = points[i]!.stats.segmentId;
    if (id === null) {
      continue;
    }
    const last = spans[spans.length - 1];
    if (last !== undefined && last.segmentId === id && last.endIndex === i - 1) {
      last.endIndex = i;
    } else {
      spans.push({ startIndex: i, endIndex: i, segmentId: id });
    }
  }
  return spans;
}

export interface TrendTooltip {
  title: string;
  lines: string[];
  metadata: string[];
}

const metadataDisplayLimit = 6;

function metadataValue(value: unknown): string {
  if (typeof value === "string") return value;
  if (value === null || typeof value === "number" || typeof value === "boolean") {
    return String(value);
  }
  return JSON.stringify(value) ?? String(value);
}

function boundaryMetadata(p: SeriesPoint): string[] {
  if (p.changeAnnotations["begins_distribution_change"] !== true) return [];
  const entries = [
    ...Object.entries(p.runTags).map(([key, value]) => `run: ${key}=${metadataValue(value)}`),
    ...Object.entries(p.info).map(([key, value]) => `info: ${key}=${metadataValue(value)}`),
  ].sort();
  if (entries.length <= metadataDisplayLimit) return entries;
  return [
    ...entries.slice(0, metadataDisplayLimit),
    `… +${entries.length - metadataDisplayLimit} more`,
  ];
}

/** deviationText gives a point's distance from its rolling mean in standard
 * deviations, with its direction in words: better or worse when the unit's
 * direction is known, else above or below. A bare z would read oppositely
 * here, where it follows the value, and in CI reports, where positive means
 * better. */
export function deviationText(z: number | null, lessIsBetter: boolean | null): string | null {
  if (z === null) return null;
  const size = `${Math.abs(z).toFixed(2)}σ`;
  if (size === "0.00σ") return size;
  if (lessIsBetter === null) return `${size} ${z > 0 ? "above" : "below"}`;
  const worse = lessIsBetter ? z > 0 : z < 0;
  return `${size} ${worse ? "worse" : "better"}`;
}

/** pointTooltip is the hover view-model. The title keeps the walking-skeleton
 * `sha · value unit` shape (the keystone e2e asserts it); lines add the engine
 * stats, omitting whatever is unavailable rather than printing nulls.
 * baselineCaptioned drops the selected baseline's explanation for a chart whose
 * caption already gives it. */
export function pointTooltip(
  p: SeriesPoint,
  locale?: string,
  lessIsBetter: boolean | null = null,
  baselineCaptioned = false,
): TrendTooltip {
  const lines: string[] = [];
  const formatted = formatMeasurement(p.svs, p.unit);
  const exact = exactMeasurement(p.svs, p.unit);
  if (formatted !== exact) {
    lines.push(`exact ${exact}`);
  }
  const deviation = deviationText(p.stats.z, lessIsBetter);
  if (deviation !== null) {
    lines.push(`vs. rolling mean: ${deviation}`);
  }
  if (p.stats.fixedBaseline && p.stats.rollingMean !== null) {
    const value = formatMeasurement(p.stats.rollingMean, p.unit);
    if (baselineCaptioned) {
      lines.push(p.stats.rollingStddev === null ? `selected baseline ${value}` : "selected baseline");
    } else {
      lines.push(p.stats.rollingStddev === null
        ? `selected baseline ${value}, averaged from a range that includes later results`
        : "selected baseline averaged from a range that includes later results");
    }
  }
  if (p.stats.rollingMean !== null && p.stats.rollingStddev !== null) {
    lines.push(`mean ${formatMeasurement(p.stats.rollingMean, p.unit)} · standard deviation ${formatMeasurement(p.stats.rollingStddev, p.unit)}`);
  }
  lines.push(formatDate(new Date(p.chartMs).toISOString(), locale));
  const flags = flagsText(p.stats);
  if (flags !== "") {
    lines.push(flags);
  }
  if (p.commitMessage !== "") {
    lines.push(p.commitMessage.length > 80 ? `${p.commitMessage.slice(0, 79)}…` : p.commitMessage);
  }
  return {
    title: `${p.commitHash} · ${formatted}`,
    lines,
    metadata: boundaryMetadata(p),
  };
}

/** effectiveChartMs is the timestamp a sample is plotted at: commit time when
 * present, else the result (run) time. */
export function effectiveChartMs(sample: HistorySample): number {
  return Date.parse(sample.commit_timestamp ?? sample.result_timestamp);
}

/** orderSamplesForChart sorts samples ascending by effectiveChartMs so the
 * derived chart and table share one monotonic, index-aligned ordering even
 * when some samples lack a commit timestamp. */
export function orderSamplesForChart(samples: HistorySample[]): HistorySample[] {
  return [...samples].sort((a, b) => effectiveChartMs(a) - effectiveChartMs(b));
}

/** distinctUnits returns the sorted set of units across samples. Null is a
 * distinct unit identity: mixing a unitless sample with a measured unit is no
 * more comparable than mixing two named units. */
export function distinctUnits(samples: HistorySample[]): (string | null)[] {
  const seen = new Set<string | null>();
  for (const s of samples) {
    seen.add(s.unit);
  }
  return [...seen].sort((a, b) => (a ?? "").localeCompare(b ?? ""));
}

/** BaselineOverride is the stored baseline_override change annotation. */
export interface BaselineOverride {
  value: number;
  throughResultId: string;
  resultCount: number;
}

export function baselineOverrideOf(annotations: Record<string, unknown>): BaselineOverride | null {
  const raw = annotations["baseline_override"];
  if (raw === null || typeof raw !== "object") return null;
  const o = raw as Record<string, unknown>;
  const value = o["value"];
  const through = o["through_result_id"];
  const count = o["result_count"];
  if (typeof value !== "number" || !Number.isFinite(value)) return null;
  return {
    value,
    throughResultId: typeof through === "string" ? through : "",
    resultCount: typeof count === "number" ? count : 0,
  };
}

/** BaselineRangePreview describes a range being selected as a baseline. It
 * mirrors the server: every point plotted from the first selection's time
 * through the last's, inclusive, averaged by plotted value. */
export interface BaselineRangePreview {
  /** start is the selected result that will carry the baseline. Results tied
   * with it at startIndex are averaged too but do not receive it. */
  start: SeriesPoint;
  startIndex: number;
  throughIndex: number;
  indices: number[];
  mean: number;
  /** blockedBy is the first point the server would reject the range for: a
   * later marked distribution change or selected baseline, or a selected
   * baseline on another result tied with the start. */
  blockedBy: SeriesPoint | null;
}

export function baselineRangePreview(
  points: SeriesPoint[],
  a: number,
  b: number,
): BaselineRangePreview | null {
  if (a < 0 || b < 0) return null;
  const first = points[Math.min(a, b)];
  const last = points[Math.max(a, b)];
  if (first === undefined || last === undefined) return null;
  const indices: number[] = [];
  let sum = 0;
  let blockedBy: SeriesPoint | null = null;
  points.forEach((p, i) => {
    if (p.chartMs < first.chartMs || p.chartMs > last.chartMs) return;
    indices.push(i);
    sum += p.svs;
    const selected = baselineOverrideOf(p.changeAnnotations) !== null;
    const crossed = p.chartMs > first.chartMs && (selected || p.changeAnnotations["begins_distribution_change"] === true);
    const tiedConflict = p.chartMs === first.chartMs && p !== first && selected;
    if (blockedBy === null && (crossed || tiedConflict)) blockedBy = p;
  });
  return {
    start: first,
    startIndex: indices[0]!,
    throughIndex: indices[indices.length - 1]!,
    indices,
    mean: sum / indices.length,
    blockedBy,
  };
}

/** SelectedBaselineSpan is a stored selected baseline as plotted: the run of
 * points it applies to (start..end) and, when its first result is plotted, the
 * range it was averaged from (start..through). */
export interface SelectedBaselineSpan {
  startIndex: number;
  throughIndex: number | null;
  endIndex: number;
  /** value is null only when the span's start is not plotted and every
   * plotted point in it is an outlier. */
  value: number | null;
  override: BaselineOverride | null;
}

export function selectedBaselineSpans(points: SeriesPoint[]): SelectedBaselineSpan[] {
  const spans: SelectedBaselineSpan[] = [];
  for (let i = 0; i < points.length; i++) {
    const p = points[i]!;
    if (!p.stats.fixedBaseline) continue;
    const last = spans[spans.length - 1];
    if (last !== undefined && last.endIndex === i - 1 && points[last.startIndex]!.stats.segmentId === p.stats.segmentId) {
      last.endIndex = i;
      if (last.value === null && p.stats.rollingMean !== null) last.value = p.stats.rollingMean;
      continue;
    }
    // Results tied at a commit share a segment, and any of them can carry its baseline.
    const carrier = points.find((q) => q.chartMs === p.chartMs && baselineOverrideOf(q.changeAnnotations) !== null);
    const override = carrier === undefined ? null : baselineOverrideOf(carrier.changeAnnotations);
    const through = override === null ? -1 : points.findIndex((q) => q.resultId === override.throughResultId);
    spans.push({
      startIndex: i,
      throughIndex: through < 0 ? null : through,
      endIndex: i,
      value: override?.value ?? p.stats.rollingMean,
      override,
    });
  }
  return spans;
}
