import type { createBenchDBClient } from "../api/client";
import type { components } from "../api/schema";
import {
  distinctUnits,
  orderSamplesForChart,
  toSeriesPoints,
  type SeriesPoint,
} from "./transform";

type Client = ReturnType<typeof createBenchDBClient>;
type BenchmarkHistory = components["schemas"]["BenchmarkHistory"];
type BenchmarkSegment = components["schemas"]["BenchmarkSegment"];
type HistorySample = components["schemas"]["HistorySample"];
type SeriesListItem = components["schemas"]["SeriesListItem"];

export interface SeriesIdentity {
  benchmarkId: string;
  displayBenchmarkId: string;
  benchmarkName: string;
  caseTags: Record<string, unknown>;
  repository: string;
  repositoryLabel: string;
  unit: string | null;
  lessIsBetter: boolean | null;
}

export interface MachineSegment {
  fingerprint: string;
  context: Record<string, unknown>;
  hardware: BenchmarkSegment["hardware"];
  points: SeriesPoint[];
}

export interface MachineTrack {
  machineName: string;
  segments: MachineSegment[];
  points: SeriesPoint[];
}

/** TrendSource enters by stable benchmark id, a result whose benchmark id is
 * resolved first, or the shipped fingerprint route for one comparable segment. */
export type TrendSource =
  | { kind: "result"; resultId: string }
  | { kind: "benchmark"; benchmarkId: string }
  | { kind: "fingerprint"; fingerprint: string };

export interface TrendViewModel {
  identity: SeriesIdentity;
  tracks: MachineTrack[];
  units: (string | null)[];
  unitConsistent: boolean;
}

function compactIdentifier(value: string, head: number, tail: number): string {
  if (value.length <= head + tail + 1) return value;
  return `${value.slice(0, head)}…${value.slice(-tail)}`;
}

function formatRepositoryLabel(repository: string): string {
  if (repository === "") return "repository not set";
  let u: URL;
  try {
    u = new URL(repository);
  } catch {
    return repository;
  }
  const path = u.pathname.replace(/^\/+|\/+$/g, "");
  if (u.hostname === "github.com" || u.hostname === "www.github.com") {
    return path || u.hostname;
  }
  return `${u.hostname}${path === "" ? "" : `/${path}`}`;
}

function segmentPoints(samples: HistorySample[] | null): SeriesPoint[] {
  return toSeriesPoints(orderSamplesForChart(samples ?? []));
}

function assemble(history: BenchmarkHistory): TrendViewModel {
  const tags: Record<string, unknown> = { ...history.tags };
  delete tags["name"];
  const tracks = (history.tracks ?? []).map((track): MachineTrack => {
    const segments = (track.segments ?? []).map((segment): MachineSegment => ({
      fingerprint: segment.history_fingerprint,
      context: segment.context,
      hardware: segment.hardware,
      points: segmentPoints(segment.samples),
    }));
    const points = segments
      .flatMap((segment) => segment.points)
      .sort((a, b) => a.chartMs - b.chartMs || a.resultId.localeCompare(b.resultId));
    return { machineName: track.machine_name, segments, points };
  });
  const samples = (history.tracks ?? []).flatMap((track) =>
    (track.segments ?? []).flatMap((segment) => segment.samples ?? []),
  );
  const units = distinctUnits(samples);
  return {
    identity: {
      benchmarkId: history.benchmark_id,
      displayBenchmarkId: compactIdentifier(history.benchmark_id, 12, 8),
      benchmarkName: history.name,
      caseTags: tags,
      repository: history.repository,
      repositoryLabel: formatRepositoryLabel(history.repository),
      unit: history.unit,
      lessIsBetter: history.less_is_better,
    },
    tracks,
    units,
    unitConsistent: units.length <= 1,
  };
}

function assembleFingerprint(item: SeriesListItem, samples: HistorySample[] | null): TrendViewModel {
  const tags: Record<string, unknown> = { ...item.tags };
  delete tags["name"];
  const points = segmentPoints(samples);
  const units = distinctUnits(samples ?? []);
  return {
    identity: {
      benchmarkId: item.history_fingerprint,
      displayBenchmarkId: compactIdentifier(item.history_fingerprint, 12, 8),
      benchmarkName: item.name,
      caseTags: tags,
      repository: item.repository,
      repositoryLabel: formatRepositoryLabel(item.repository),
      unit: item.unit,
      lessIsBetter: item.less_is_better,
    },
    tracks: [{
      machineName: item.hardware.name,
      segments: [{
        fingerprint: item.history_fingerprint,
        context: item.context,
        hardware: item.hardware,
        points,
      }],
      points,
    }],
    units,
    unitConsistent: units.length <= 1,
  };
}

async function loadByBenchmark(client: Client, benchmarkId: string): Promise<TrendViewModel> {
  const res = await client.GET("/api/benchmarks/{benchmark_id}", {
    params: { path: { benchmark_id: benchmarkId } },
    cache: "no-store",
  });
  if (res.error || !res.data) {
    if (res.response?.status === 404) {
      throw new Error(`benchmark ${benchmarkId} has no comparable default-branch history`);
    }
    throw new Error(`failed to load benchmark ${benchmarkId}`);
  }
  return assemble(res.data);
}

async function loadByFingerprint(client: Client, fingerprint: string): Promise<TrendViewModel> {
  const [historyRes, seriesRes] = await Promise.all([
    client.GET("/api/history", { params: { query: { fingerprint } }, cache: "no-store" }),
    client.GET("/api/series", { params: { query: { fingerprint, page_size: 1 } }, cache: "no-store" }),
  ]);
  if (historyRes.error || !historyRes.data) {
    throw new Error(`failed to load history for series ${fingerprint}`);
  }
  if (seriesRes.error || !seriesRes.data) {
    throw new Error(`failed to load series ${fingerprint}`);
  }
  const item = seriesRes.data.series?.[0];
  if (item === undefined) {
    throw new Error(`series ${fingerprint} not found`);
  }
  return assembleFingerprint(item, historyRes.data.samples);
}

async function loadByResult(client: Client, resultId: string): Promise<TrendViewModel> {
  const detailRes = await client.GET("/api/benchmark-results/{id}", {
    params: { path: { id: resultId } },
  });
  if (detailRes.error || !detailRes.data) {
    throw new Error(`failed to load benchmark result ${resultId}`);
  }
  if (detailRes.data.commit === null) {
    throw new Error(`benchmark result ${resultId} has no comparable default-branch history`);
  }
  return loadByBenchmark(client, detailRes.data.benchmark_id);
}

export async function loadTrend(client: Client, source: TrendSource): Promise<TrendViewModel> {
  switch (source.kind) {
    case "result":
      return loadByResult(client, source.resultId);
    case "fingerprint":
      return loadByFingerprint(client, source.fingerprint);
    case "benchmark":
      return loadByBenchmark(client, source.benchmarkId);
  }
}
