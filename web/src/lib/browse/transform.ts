import type { BenchmarkListItem } from "../api/benchdb";
import { formatMeasurement, formatNumber } from "../format";
import { repositoryLabel } from "../repository";
import type { BrowseWindow } from "../router";

/** SeriesStatus is the wire enum (closed union in the generated schema). */
export type SeriesStatus = BenchmarkListItem["status"];

export interface BrowseRow {
  benchmarkId: string;
  name: string;
  repository: string;
  paramsText: string;
  machineNames: string[];
  latestSVS: number | null;
  unit: string | null;
  svsText: string;
  pointCount: number;
  status: SeriesStatus;
  commitSha: string;
  commitTimestampMs: number;
  commitDateText: string;
  previewTracks: BrowsePreviewTrack[];
}

export interface BrowsePreviewPoint {
  chartMs: number;
  value: number;
  unit: string | null;
}

export interface BrowsePreviewTrack {
  machineName: string;
  points: BrowsePreviewPoint[];
}

/** formatSVS renders an SVS for dense tables: exact grouped integers and
 * compact decimal values with trailing zeros trimmed. */
export function formatSVS(value: number): string {
  return formatNumber(value);
}

/** formatDate renders a short local date; datetimes are UTC on the wire and
 * local-time conversion belongs here in the UI. locale is injectable for tests. */
export function formatDate(iso: string, locale?: string): string {
  return new Intl.DateTimeFormat(locale, { year: "numeric", month: "short", day: "numeric" }).format(
    new Date(iso),
  );
}

/** tagsText joins tags as `k=v · k2=v2`, keys sorted for determinism. */
export function tagsText(tags: Record<string, unknown>, omit: string[] = []): string {
  const skip = new Set(omit);
  return Object.keys(tags)
    .filter((k) => !skip.has(k))
    .sort()
    .map((k) => `${k}=${String(tags[k])}`)
    .join(" · ");
}

export function toBrowseRows(items: BenchmarkListItem[], locale?: string): BrowseRow[] {
  return items.map((item) => {
    const svs = item.latest_single_value_summary;
    return {
      benchmarkId: item.benchmark_id,
      name: item.name,
      repository: item.repository,
      paramsText: tagsText(item.tags, ["name"]),
      machineNames: item.machine_names ?? [],
      latestSVS: svs,
      unit: item.unit,
      svsText: formatMeasurement(svs, item.unit),
      pointCount: item.point_count,
      status: item.status,
      commitSha: item.latest_commit_sha.slice(0, 7),
      commitTimestampMs: Date.parse(item.latest_commit_timestamp),
      commitDateText: formatDate(item.latest_commit_timestamp, locale),
      previewTracks: (item.preview_tracks ?? []).map((track) => ({
        machineName: track.machine_name,
        points: (track.points ?? []).map((point) => ({
          chartMs: Date.parse(point.commit_timestamp),
          value: point.value,
          unit: point.unit,
        })),
      })),
    };
  });
}

export type SortKey = "name" | "svs" | "points" | "commit";

export interface SortSpec {
  key: SortKey;
  dir: "asc" | "desc";
}

const SORT_ACCESSORS: Record<SortKey, (r: BrowseRow) => string | number | null> = {
  name: (r) => r.name,
  svs: (r) => r.latestSVS,
  points: (r) => r.pointCount,
  commit: (r) => r.commitTimestampMs,
};

const STATUS_ATTENTION: Record<SeriesStatus, number> = {
  regressed: 0,
  improved: 1,
  stable: 2,
  insufficient: 3,
};

/** sortRows orders the LOADED rows client-side. The default (null) puts
 * regressions first, then improvements, then the rest, each by name, so
 * related benchmarks such as `*-wall-time` and `*-cpu-time` stay together.
 * A null SVS sorts last in both directions. Pure: returns a copy. */
export function sortRows(rows: BrowseRow[], sort: SortSpec | null): BrowseRow[] {
  if (sort === null) {
    return [...rows].sort((a, b) =>
      STATUS_ATTENTION[a.status] - STATUS_ATTENTION[b.status] || a.name.localeCompare(b.name),
    );
  }
  const get = SORT_ACCESSORS[sort.key];
  const dir = sort.dir === "asc" ? 1 : -1;
  return [...rows].sort((a, b) => {
    const av = get(a);
    const bv = get(b);
    if (av === null) return bv === null ? 0 : 1;
    if (bv === null) return -1;
    if (typeof av === "string" && typeof bv === "string") return dir * av.localeCompare(bv);
    return dir * ((av as number) - (bv as number));
  });
}

const WINDOW_DAYS: Record<Exclude<BrowseWindow, "all">, number> = {
  "30d": 30,
  "3mo": 90,
  "1y": 365,
};

/** windowStartIso turns a window preset into the absolute UTC active_since the
 * endpoint takes; "all" means unconstrained (null). Presets are FIXED-LENGTH
 * rolling windows (30/90/365 days ending at now), not calendar-aligned months
 * or years: deterministic, DST-free, and shared with the trend's range presets
 * (windowPoints in series/transform.ts). now is injectable. */
export function windowStartIso(window: BrowseWindow, now: Date): string | null {
  if (window === "all") {
    return null;
  }
  return new Date(now.getTime() - WINDOW_DAYS[window] * 24 * 60 * 60 * 1000).toISOString();
}

/** sparklinePoints maps values onto an SVG polyline points string inside a
 * width x height box with pad margins; min/max normalized, larger = higher
 * (smaller y). A flat series renders as a midline. */
export function sparklinePoints(values: number[], width: number, height: number, pad: number): string {
  if (values.length === 0) {
    return "";
  }
  const min = Math.min(...values);
  const max = Math.max(...values);
  const span = max - min;
  const innerW = width - 2 * pad;
  const innerH = height - 2 * pad;
  return values
    .map((v, i) => {
      const x = values.length === 1 ? width / 2 : pad + (i * innerW) / (values.length - 1);
      const y = span === 0 ? height / 2 : pad + innerH - ((v - min) / span) * innerH;
      return `${round2(x)},${round2(y)}`;
    })
    .join(" ");
}

function round2(n: number): number {
  return Math.round(n * 100) / 100;
}

export interface BrowseGroup {
  repository: string;
  label: string;
  rows: BrowseRow[];
}

/** groupByRepository splits ordered rows into one group per repository,
 * groups sorted by label, keeping each group's row order. */
export function groupByRepository(rows: BrowseRow[]): BrowseGroup[] {
  const groups = new Map<string, BrowseGroup>();
  for (const row of rows) {
    let group = groups.get(row.repository);
    if (group === undefined) {
      group = { repository: row.repository, label: repositoryLabel(row.repository), rows: [] };
      groups.set(row.repository, group);
    }
    group.rows.push(row);
  }
  return [...groups.values()].sort((a, b) => a.label.localeCompare(b.label));
}

export interface StatusCounts {
  regressed: number;
  improved: number;
}

export function statusCounts(rows: BrowseRow[]): StatusCounts {
  return {
    regressed: rows.filter((row) => row.status === "regressed").length,
    improved: rows.filter((row) => row.status === "improved").length,
  };
}
