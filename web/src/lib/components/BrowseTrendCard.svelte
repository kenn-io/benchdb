<script lang="ts">
  import { appURL } from "../base-path";
  import { createBenchDBClient } from "../api/client";
  import { formatMeasurement } from "../format";
  import {
    comparePreviewPoints,
    latestPreviewPoint,
    trackSummary,
    type BrowsePreviewPoint,
    type BrowsePreviewTrack,
    type BrowseRow,
  } from "../browse/transform";
  import { machineColors } from "../machine-colors";
  import { axisTickLabels } from "../series/chart-format";
  import { loadTrend } from "../series/loader";
  import {
    niceTicks,
    observedValueRange,
    tooltipLeftForCursor,
    tooltipTopForCursor,
    zeroBasedValueRange,
    type ValueRange,
  } from "../series/chart-geometry";
  import { resolvedTheme } from "../theme.svelte";
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

  // The SVG uses the card's pixel width as its coordinate space, so text and
  // dots keep their size at any card width.
  const DEFAULT_WIDTH = 520;
  const HEIGHT = 150;
  const PAD_RIGHT = 10;
  const PLOT_TOP = 10;
  const PLOT_BOTTOM = 124;
  const AXIS_LABEL_Y = 143;
  const LABEL_CHAR_WIDTH = 6;
  // Below this many pixels per point, dots merge into a bar; draw only the line.
  const MIN_DOT_SPACING = 10;

  let hovered = $state<{ machineName: string; point: BrowsePreviewPoint } | null>(null);
  let measuredWidth = $state(0);
  let tooltipWidth = $state(0);
  let tooltipHeight = $state(0);
  const width = $derived(measuredWidth > 0 ? measuredWidth : DEFAULT_WIDTH);
  let historyTracks = $state<BrowsePreviewTrack[] | null>(null);
  let loadingHistory = $state(false);
  let historyError = $state<string | null>(null);
  const tracks = $derived(historyTracks ?? row.previewTracks);

  // A page can hold hundreds of cards, so each card fetches its full history
  // only when it comes near the viewport. The page scrolls inside
  // .app-content, which clips anything outside it, so the preload margin must
  // apply to that container rather than to the window.
  let card = $state<HTMLElement>();
  let nearViewport = $state(false);
  $effect(() => {
    if (card === undefined || nearViewport) return;
    const observer = new IntersectionObserver((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) nearViewport = true;
    }, { root: card.closest(".app-content"), rootMargin: "600px 0px" });
    observer.observe(card);
    return () => observer.disconnect();
  });

  $effect(() => {
    const current = row;
    historyTracks = null;
    historyError = null;
    hovered = null;
    const incomplete = current.previewTracks.reduce((count, track) => count + track.points.length, 0) < current.pointCount;
    loadingHistory = incomplete;
    if (!incomplete || !nearViewport) return;

    let active = true;
    void loadTrend(createBenchDBClient(baseUrl), { kind: "benchmark", benchmarkId: current.benchmarkId })
      .then((history) => {
        if (!active) return;
        historyTracks = history.tracks
          .filter((track) => current.machineNames.includes(track.machineName))
          .map((track) => ({
            machineName: track.machineName,
            points: track.segments.flatMap((segment) => segment.points)
              .map((point) => ({ chartMs: point.chartMs, resultId: point.resultId, value: point.svs, unit: point.unit }))
              .sort(comparePreviewPoints),
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
  let yTicks = $derived(yRange === null ? [] : niceTicks(yRange));
  let yLabels = $derived(axisTickLabels(yTicks, allPoints[0]?.unit ?? row.unit));
  let plotLeft = $derived(Math.max(...yLabels.map((label) => label.length), 1) * LABEL_CHAR_WIDTH + 12);
  let plotRight = $derived(width - PAD_RIGHT);
  let colors = $derived(machineColors(tracks.map((track) => track.machineName), resolvedTheme()));
  // The legend's latest values come from the points inside the time window,
  // the same points the chart marks and the summary describes.
  let latestByMachine = $derived(
    new Map(visibleTracks.map((track) => [track.machineName, latestPreviewPoint(track.points)])),
  );

  function x(point: BrowsePreviewPoint): number {
    if (maxX === minX) return (plotLeft + plotRight) / 2;
    return plotLeft + ((point.chartMs - minX) / (maxX - minX)) * (plotRight - plotLeft);
  }

  function yFor(value: number): number {
    if (yRange === null) return PLOT_BOTTOM;
    const span = yRange.max - yRange.min || 1;
    return PLOT_BOTTOM - ((value - yRange.min) / span) * (PLOT_BOTTOM - PLOT_TOP);
  }

  function y(point: BrowsePreviewPoint): number {
    return yFor(point.value);
  }

  function path(points: BrowsePreviewPoint[]): string {
    return points.map((point, index) => `${index === 0 ? "M" : "L"}${x(point).toFixed(2)},${y(point).toFixed(2)}`).join(" ");
  }

  function showDots(points: BrowsePreviewPoint[]): boolean {
    return points.length <= (plotRight - plotLeft) / MIN_DOT_SPACING;
  }

  function pointTitle(machineName: string, point: BrowsePreviewPoint): string {
    return `${machineName} · ${new Date(point.chartMs).toLocaleString()} · ${formatMeasurement(point.value, point.unit)}`;
  }

  // Screen readers get the chart's content as text: per machine, the latest
  // value and the range and count of the points shown. When the chart shows a
  // notice instead of points, the summary gives that notice.
  function chartSummary(): string {
    const open = `Open trend ${row.name}`;
    if (loadingHistory) return `${open}. Loading history`;
    if (historyError) return `${open}. History unavailable`;
    if (allPoints.length === 0) return `${open}. No trend preview`;
    if (previewUnitCount > 1) return `${open}. Preview unavailable: mixed units`;
    const parts = visibleTracks.map(trackSummary).filter((part) => part !== null);
    return [open, ...parts].join(". ");
  }

  function axisDate(chartMs: number): string {
    return new Intl.DateTimeFormat(undefined, {
      month: "short", day: "numeric",
      ...(maxX - minX < 86_400_000 ? { hour: "numeric", minute: "2-digit" } as const : {}),
    }).format(chartMs);
  }

  // Keep the tooltip inside the chart: beside the point, held back from the
  // side edges, and below the point when there is no room above it.
  function tooltipStyle(point: BrowsePreviewPoint): string {
    const left = tooltipLeftForCursor(x(point), width, tooltipWidth || 220, 6);
    const top = tooltipTopForCursor(y(point), HEIGHT, tooltipHeight || 32, 6).top;
    return `left:${left}px;top:${top}px`;
  }
</script>

<article class="trend-card panel" bind:this={card}>
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

  <button type="button" class="chart-button" aria-label={chartSummary()} onclick={() => onopen?.(row)}
    bind:clientWidth={measuredWidth}>
    {#if loadingHistory}
      <span class="no-preview" aria-live="polite">Loading history…</span>
    {:else if historyError}
      <span class="no-preview" role="alert">{historyError}</span>
    {:else if allPoints.length === 0}
      <span class="no-preview">No trend preview</span>
    {:else if previewUnitCount > 1}
      <span class="no-preview">Preview unavailable: mixed units</span>
    {:else}
      <svg viewBox={`0 0 ${width} ${HEIGHT}`} width={width} height={HEIGHT} aria-hidden="true">
        {#each yTicks as tick, index (tick)}
          <line class="grid" x1={plotLeft} y1={yFor(tick)} x2={plotRight} y2={yFor(tick)} />
          <text class="value-label" x={plotLeft - 6} y={yFor(tick)} text-anchor="end"
            dominant-baseline="middle">{yLabels[index]}</text>
        {/each}
        <line class="axis" x1={plotLeft} y1={PLOT_BOTTOM} x2={plotRight} y2={PLOT_BOTTOM} />
        <text class="axis-label" x={plotLeft} y={AXIS_LABEL_Y} text-anchor="start">{axisDate(minX)}</text>
        {#if maxX !== minX}
          <text class="axis-label" x={plotRight} y={AXIS_LABEL_Y} text-anchor="end">{axisDate(maxX)}</text>
        {/if}
        {#each visibleTracks as track (track.machineName)}
          {@const color = colors.get(track.machineName)}
          {@const latest = latestPreviewPoint(track.points)}
          <path d={path(track.points)} style={`stroke:${color}`} />
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
            {#if showDots(track.points) && point !== latest}
              <circle class="point-mark" cx={x(point)} cy={y(point)} r="2.75" style={`fill:${color}`} />
            {/if}
          {/each}
          {#if latest !== undefined}
            <circle class="point-mark latest" cx={x(latest)} cy={y(latest)} r="4" style={`fill:${color}`} />
          {/if}
        {/each}
      </svg>
      {#if hovered}
        <span class="chart-tooltip" role="tooltip" style={tooltipStyle(hovered.point)}
          bind:clientWidth={tooltipWidth} bind:clientHeight={tooltipHeight}>
          {pointTitle(hovered.machineName, hovered.point)}
        </span>
      {/if}
    {/if}
  </button>

  <footer>
    {#each tracks as track (track.machineName)}
      {@const latest = latestByMachine.get(track.machineName)}
      <span class="machine">
        <i style={`background:${colors.get(track.machineName)}`}></i>
        <span class="machine-name">{track.machineName}</span>
        <strong><MeasurementValue value={latest?.value ?? null} unit={latest?.unit ?? row.unit} /></strong>
      </span>
    {/each}
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
  path { fill: none; stroke-width: 2; stroke-linejoin: round; }
  .axis { stroke: var(--c-border); stroke-width: 1; }
  .grid { stroke: var(--c-border-muted); stroke-width: 1; }
  .axis-label, .value-label { fill: var(--c-text-muted); font-size: 10px; font-variant-numeric: tabular-nums; }
  .point-hit { fill: transparent; pointer-events: all; }
  .point-mark { pointer-events: none; }
  .point-mark.latest { stroke: var(--c-chart-bg); stroke-width: 1.5; }
  .chart-tooltip {
    position: absolute;
    z-index: 2;
    max-width: min(280px, calc(100% - 12px));
    padding: 5px 7px;
    border: 1px solid var(--c-border);
    border-radius: var(--radius-sm);
    background: var(--c-surface);
    box-shadow: var(--shadow-md);
    color: var(--c-text);
    font-size: 0.72rem;
    font-variant-numeric: tabular-nums;
    line-height: 1.25;
    text-align: left;
    pointer-events: none;
  }
  .no-preview { display: grid; place-items: center; min-height: 150px; }
  footer { flex-wrap: wrap; justify-content: flex-start; gap: 4px 16px; }
  .machine { display: inline-flex; align-items: baseline; gap: 6px; min-width: 0; font-size: 0.72rem; }
  .machine i { display: inline-block; flex: 0 0 auto; width: 10px; height: 3px; border-radius: 999px; align-self: center; }
  .machine-name { color: var(--c-text-muted); overflow-wrap: anywhere; }
  .machine strong { font-size: 0.82rem; font-variant-numeric: tabular-nums; }
</style>
