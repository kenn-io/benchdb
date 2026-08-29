<script lang="ts">
  import { formatMeasurement } from "../format";
  import type { BrowsePreviewPoint, BrowseRow } from "../browse/transform";
  import MeasurementValue from "./MeasurementValue.svelte";
  import StatusBadge from "./StatusBadge.svelte";

  let {
    row,
    onopen,
  }: {
    row: BrowseRow;
    onopen?: (row: BrowseRow) => void;
  } = $props();

  const WIDTH = 520;
  const HEIGHT = 150;
  const PAD_X = 8;
  const PAD_Y = 8;
  const palette = ["#2563eb", "#dc2626", "#059669", "#d97706", "#7c3aed", "#0891b2"];

  let allPoints = $derived(row.previewTracks.flatMap((track) => track.points));
  let minX = $derived(allPoints.length === 0 ? 0 : Math.min(...allPoints.map((point) => point.chartMs)));
  let maxX = $derived(allPoints.length === 0 ? 1 : Math.max(...allPoints.map((point) => point.chartMs)));
  let maxY = $derived(Math.max(1, ...allPoints.map((point) => point.value)) * 1.05);

  function x(point: BrowsePreviewPoint): number {
    if (maxX === minX) return WIDTH / 2;
    return PAD_X + ((point.chartMs - minX) / (maxX - minX)) * (WIDTH - PAD_X * 2);
  }

  function y(point: BrowsePreviewPoint): number {
    return HEIGHT - PAD_Y - (point.value / maxY) * (HEIGHT - PAD_Y * 2);
  }

  function path(points: BrowsePreviewPoint[]): string {
    return points.map((point, index) => `${index === 0 ? "M" : "L"}${x(point).toFixed(2)},${y(point).toFixed(2)}`).join(" ");
  }

  function pointTitle(machineName: string, point: BrowsePreviewPoint): string {
    return `${machineName} · ${new Date(point.chartMs).toLocaleDateString()} · ${formatMeasurement(point.value, row.unit)}`;
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
        <line class="axis" x1={PAD_X} y1={HEIGHT - PAD_Y} x2={WIDTH - PAD_X} y2={HEIGHT - PAD_Y} />
        {#each row.previewTracks as track, trackIndex (track.machineName)}
          <path d={path(track.points)} stroke={palette[trackIndex % palette.length]} />
          {#each track.points as point, pointIndex (`${point.chartMs}-${pointIndex}`)}
            <circle cx={x(point)} cy={y(point)} r="2.5" fill={palette[trackIndex % palette.length]}>
              <title>{pointTitle(track.machineName, point)}</title>
            </circle>
          {/each}
        {/each}
      </svg>
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
  }
  .chart-button:hover { border-color: var(--c-accent); }
  svg { display: block; width: 100%; height: 150px; }
  path { fill: none; stroke-width: 2; vector-effect: non-scaling-stroke; }
  .axis { stroke: var(--c-border); stroke-width: 1; vector-effect: non-scaling-stroke; }
  .no-preview { display: grid; place-items: center; min-height: 150px; }
  .machines { display: flex; flex-wrap: wrap; gap: 5px 10px; color: var(--c-text-muted); font-size: 0.7rem; }
  .machines span { display: inline-flex; align-items: center; gap: 4px; }
  .machines i { display: inline-block; width: 10px; height: 2px; border-radius: 999px; }
  footer strong { flex: 0 0 auto; font-variant-numeric: tabular-nums; }
</style>
