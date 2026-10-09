<script lang="ts">
  import { appURL } from "../base-path";
  import { createBenchDBClient } from "../api/client";
  import { formatMeasurement } from "../format";
  import type { BrowsePreviewPoint, BrowsePreviewTrack, BrowseRow } from "../browse/transform";
  import { machineColor } from "../machine-colors";
  import { loadTrend } from "../series/loader";
  import { observedValueRange, zeroBasedValueRange, type ValueRange } from "../series/chart-geometry";
  import MeasurementValue from "./MeasurementValue.svelte";
  import StatusBadge from "./StatusBadge.svelte";

  let {
    row,
    baseUrl = "",
    zeroBased = false,
    timeRange = null,
    onopen,
  }: {
    row: BrowseRow;
    baseUrl?: string;
    zeroBased?: boolean;
    timeRange?: ValueRange | null;
    onopen?: (row: BrowseRow) => void;
  } = $props();

  const WIDTH = 520;
  const HEIGHT = 150;
  const PAD_X = 8;
  const PLOT_TOP = 8;
  const PLOT_BOTTOM = 126;
  const AXIS_LABEL_Y = 145;

  let hovered = $state<{ machineName: string; point: BrowsePreviewPoint } | null>(null);
  let historyTracks = $state<BrowsePreviewTrack[] | null>(null);
  let loadingHistory = $state(false);
  let historyError = $state<string | null>(null);
  const tracks = $derived(historyTracks ?? row.previewTracks);

  $effect(() => {
    const current = row;
    historyTracks = null;
    historyError = null;
    hovered = null;
    const incomplete = current.previewTracks.reduce((count, track) => count + track.points.length, 0) < current.pointCount;
    loadingHistory = incomplete;
    if (!incomplete) return;

    let active = true;
    void loadTrend(createBenchDBClient(baseUrl), { kind: "benchmark", benchmarkId: current.benchmarkId })
      .then((history) => {
        if (!active) return;
        historyTracks = history.tracks
          .filter((track) => current.machineNames.includes(track.machineName))
          .map((track) => ({
            machineName: track.machineName,
            points: track.segments.flatMap((segment) => segment.points)
              .map((point) => ({ chartMs: point.chartMs, value: point.svs, unit: point.unit }))
              .sort((a, b) => a.chartMs - b.chartMs),
          }));
      })
      .catch((err) => {
        if (active) historyError = err instanceof Error ? err.message : String(err);
      })
      .finally(() => {
        if (active) loadingHistory = false;
      });
    return () => { active = false; };
  });

  let visibleTracks = $derived(tracks.map((track) => ({
    ...track,
    points: timeRange === null ? track.points : track.points.filter((point) =>
      point.chartMs >= timeRange!.min && point.chartMs <= timeRange!.max,
    ),
  })));
  let allPoints = $derived(visibleTracks.flatMap((track) => track.points));
  let previewUnitCount = $derived(new Set(allPoints.map((point) => point.unit)).size);
  // The window filters the history; its empty time should not squeeze the data.
  let minX = $derived(allPoints.reduce((min, point) => Math.min(min, point.chartMs), Infinity));
  let maxX = $derived(allPoints.reduce((max, point) => Math.max(max, point.chartMs), -Infinity));
  let yRange = $derived(
    (zeroBased ? zeroBasedValueRange : observedValueRange)(allPoints.map((point) => point.value)),
  );

  function x(point: BrowsePreviewPoint): number {
    if (maxX === minX) return WIDTH / 2;
    return PAD_X + ((point.chartMs - minX) / (maxX - minX)) * (WIDTH - PAD_X * 2);
  }

  function y(point: BrowsePreviewPoint): number {
    if (yRange === null) return PLOT_BOTTOM;
    const span = yRange.max - yRange.min || 1;
    return PLOT_BOTTOM - ((point.value - yRange.min) / span) * (PLOT_BOTTOM - PLOT_TOP);
  }

  function path(points: BrowsePreviewPoint[]): string {
    return points.map((point, index) => `${index === 0 ? "M" : "L"}${x(point).toFixed(2)},${y(point).toFixed(2)}`).join(" ");
  }

  function pointTitle(machineName: string, point: BrowsePreviewPoint): string {
    return `${machineName} · ${new Date(point.chartMs).toLocaleString()} · ${formatMeasurement(point.value, point.unit)}`;
  }

  function axisDate(chartMs: number): string {
    return new Intl.DateTimeFormat(undefined, {
      month: "short", day: "numeric",
      ...(maxX - minX < 86_400_000 ? { hour: "numeric", minute: "2-digit" } as const : {}),
    }).format(chartMs);
  }

  function tooltipStyle(point: BrowsePreviewPoint): string {
    const left = Math.max(14, Math.min(86, (x(point) / WIDTH) * 100));
    const top = Math.max(24, (y(point) / HEIGHT) * 100);
    return `left:${left}%;top:${top}%`;
  }
</script>

<article class="trend-card panel">
  <header>
    <div class="identity">
      <a href={appURL(`/benchmarks/${row.benchmarkId}`)} onclick={(event) => {
        if (!onopen) return;
        event.preventDefault();
        onopen(row);
      }}>{row.name}</a>
      {#if row.paramsText}<span>{row.paramsText}</span>{/if}
    </div>
    <StatusBadge status={row.status} />
  </header>

  <button type="button" class="chart-button" aria-label={`Open trend ${row.name}`} onclick={() => onopen?.(row)}>
    {#if loadingHistory}
      <span class="no-preview" aria-live="polite">Loading history…</span>
    {:else if historyError}
      <span class="no-preview" role="alert">{historyError}</span>
    {:else if allPoints.length === 0}
      <span class="no-preview">No trend preview</span>
    {:else if previewUnitCount > 1}
      <span class="no-preview">Preview unavailable: mixed units</span>
    {:else}
      <svg viewBox={`0 0 ${WIDTH} ${HEIGHT}`} role="img" aria-label={`${row.name} fleet trend preview`}>
        <line class="axis" x1={PAD_X} y1={PLOT_BOTTOM} x2={WIDTH - PAD_X} y2={PLOT_BOTTOM} />
        <line class="axis-tick" x1={PAD_X} y1={PLOT_BOTTOM} x2={PAD_X} y2={PLOT_BOTTOM + 4} />
        <text class="axis-label" x={PAD_X} y={AXIS_LABEL_Y} text-anchor="start">{axisDate(minX)}</text>
        {#if maxX !== minX}
          <line class="axis-tick" x1={WIDTH - PAD_X} y1={PLOT_BOTTOM} x2={WIDTH - PAD_X} y2={PLOT_BOTTOM + 4} />
          <text class="axis-label" x={WIDTH - PAD_X} y={AXIS_LABEL_Y} text-anchor="end">{axisDate(maxX)}</text>
        {/if}
        {#each visibleTracks as track, trackIndex (track.machineName)}
          <path d={path(track.points)} stroke={machineColor(trackIndex)} />
          {#each track.points as point, pointIndex (`${point.chartMs}-${pointIndex}`)}
            <circle
              class="point-hit"
              role="presentation"
              cx={x(point)}
              cy={y(point)}
              r="9"
              onpointerenter={() => (hovered = { machineName: track.machineName, point })}
              onpointerleave={() => (hovered = null)}
            >
              <title>{pointTitle(track.machineName, point)}</title>
            </circle>
            <circle class="point-mark" cx={x(point)} cy={y(point)} r="2.75" fill={machineColor(trackIndex)} />
          {/each}
        {/each}
      </svg>
      {#if hovered}
        <span class="chart-tooltip" role="tooltip" style={tooltipStyle(hovered.point)}>
          {pointTitle(hovered.machineName, hovered.point)}
        </span>
      {/if}
    {/if}
  </button>

  <footer>
    <div class="machines">
      {#each tracks as track, index (track.machineName)}
        <span><i style={`background:${machineColor(index)}`}></i>{track.machineName}</span>
      {/each}
    </div>
    <strong><MeasurementValue value={row.latestSVS} unit={row.unit} /></strong>
  </footer>
</article>

<style>
  .trend-card {
    display: grid;
    gap: 8px;
    min-width: 0;
    padding: 10px;
  }
  header, footer {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 10px;
  }
  .identity { display: grid; gap: 2px; min-width: 0; }
  .identity a { color: var(--c-text); font-weight: 700; overflow-wrap: anywhere; }
  .identity span { color: var(--c-text-muted); font-size: 0.72rem; overflow-wrap: anywhere; }
  .chart-button {
    display: block;
    width: 100%;
    min-height: 150px;
    padding: 0;
    border: 1px solid var(--c-border-muted);
    border-radius: var(--radius-sm);
    background: var(--c-chart-bg);
    color: var(--c-text-muted);
    cursor: pointer;
    overflow: hidden;
    position: relative;
  }
  .chart-button:hover { border-color: var(--c-accent); }
  svg { display: block; width: 100%; height: 150px; }
  path { fill: none; stroke-width: 2; vector-effect: non-scaling-stroke; }
  .axis, .axis-tick { stroke: var(--c-border); stroke-width: 1; vector-effect: non-scaling-stroke; }
  .axis-label { fill: var(--c-text-muted); font-size: 10px; }
  .point-hit { fill: transparent; pointer-events: all; }
  .point-mark { pointer-events: none; }
  .chart-tooltip {
    position: absolute;
    z-index: 2;
    max-width: min(280px, 82%);
    padding: 5px 7px;
    border: 1px solid var(--c-border);
    border-radius: var(--radius-sm);
    background: var(--c-surface);
    box-shadow: var(--shadow-md);
    color: var(--c-text);
    font-size: 0.72rem;
    font-variant-numeric: tabular-nums;
    line-height: 1.25;
    pointer-events: none;
    transform: translate(-50%, calc(-100% - 7px));
  }
  .no-preview { display: grid; place-items: center; min-height: 150px; }
  .machines { display: flex; flex-wrap: wrap; gap: 5px 10px; color: var(--c-text-muted); font-size: 0.7rem; }
  .machines span { display: inline-flex; align-items: center; gap: 4px; }
  .machines i { display: inline-block; width: 10px; height: 2px; border-radius: 999px; }
  footer strong { flex: 0 0 auto; font-variant-numeric: tabular-nums; }
</style>
