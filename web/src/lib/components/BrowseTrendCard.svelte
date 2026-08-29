<script lang="ts">
  import { formatMeasurement } from "../format";
  import type { BrowsePreviewPoint, BrowseRow } from "../browse/transform";
  import { observedValueRange, zeroBasedValueRange } from "../series/chart-geometry";
  import MeasurementValue from "./MeasurementValue.svelte";
  import StatusBadge from "./StatusBadge.svelte";

  let {
    row,
    zeroBased = true,
    onopen,
  }: {
    row: BrowseRow;
    zeroBased?: boolean;
    onopen?: (row: BrowseRow) => void;
  } = $props();

  const WIDTH = 520;
  const HEIGHT = 150;
  const PAD_X = 8;
  const PLOT_TOP = 8;
  const PLOT_BOTTOM = 126;
  const AXIS_LABEL_Y = 145;
  const palette = ["#2563eb", "#dc2626", "#059669", "#d97706", "#7c3aed", "#0891b2"];

  let hovered = $state<{ machineName: string; point: BrowsePreviewPoint } | null>(null);
  let allPoints = $derived(row.previewTracks.flatMap((track) => track.points));
  let minX = $derived(allPoints.length === 0 ? 0 : Math.min(...allPoints.map((point) => point.chartMs)));
  let maxX = $derived(allPoints.length === 0 ? 1 : Math.max(...allPoints.map((point) => point.chartMs)));
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
    return `${machineName} · ${new Date(point.chartMs).toLocaleDateString()} · ${formatMeasurement(point.value, row.unit)}`;
  }

  function axisDate(chartMs: number): string {
    return new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" }).format(chartMs);
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
      <a href={`/series/${row.benchmarkId}`} onclick={(event) => {
        if (!onopen) return;
        event.preventDefault();
        onopen(row);
      }}>{row.name}</a>
      {#if row.paramsText}<span>{row.paramsText}</span>{/if}
    </div>
    <StatusBadge status={row.status} />
  </header>

  <button type="button" class="chart-button" aria-label={`Open trend ${row.name}`} onclick={() => onopen?.(row)}>
    {#if allPoints.length === 0}
      <span class="no-preview">No trend preview</span>
    {:else}
      <svg viewBox={`0 0 ${WIDTH} ${HEIGHT}`} role="img" aria-label={`${row.name} fleet trend preview`}>
        <line class="axis" x1={PAD_X} y1={PLOT_BOTTOM} x2={WIDTH - PAD_X} y2={PLOT_BOTTOM} />
        <line class="axis-tick" x1={PAD_X} y1={PLOT_BOTTOM} x2={PAD_X} y2={PLOT_BOTTOM + 4} />
        <text class="axis-label" x={PAD_X} y={AXIS_LABEL_Y} text-anchor="start">{axisDate(minX)}</text>
        {#if maxX !== minX}
          <line class="axis-tick" x1={WIDTH - PAD_X} y1={PLOT_BOTTOM} x2={WIDTH - PAD_X} y2={PLOT_BOTTOM + 4} />
          <text class="axis-label" x={WIDTH - PAD_X} y={AXIS_LABEL_Y} text-anchor="end">{axisDate(maxX)}</text>
        {/if}
        {#each row.previewTracks as track, trackIndex (track.machineName)}
          <path d={path(track.points)} stroke={palette[trackIndex % palette.length]} />
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
            <circle class="point-mark" cx={x(point)} cy={y(point)} r="2.75" fill={palette[trackIndex % palette.length]} />
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
      {#each row.previewTracks as track, index (track.machineName)}
        <span><i style={`background:${palette[index % palette.length]}`}></i>{track.machineName}</span>
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
