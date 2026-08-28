<script lang="ts">
  import uPlot from "uplot";
  import "uplot/dist/uPlot.min.css";
  import { onDestroy, untrack } from "svelte";

  import type { TrendAxis } from "../router";
  import { compactAxisValue } from "../series/chart-format";
  import { paddedValueRange } from "../series/chart-geometry";
  import type { MachineTrack } from "../series/loader";
  import { resolvedTheme } from "../theme.svelte";

  let {
    tracks,
    axis = "commit",
    height = 320,
  }: {
    tracks: MachineTrack[];
    axis?: TrendAxis;
    height?: number;
  } = $props();

  let host: HTMLDivElement;
  let chart: uPlot | undefined;
  let resizeObserver: ResizeObserver | undefined;

  const palette = ["#2563eb", "#dc2626", "#059669", "#d97706", "#7c3aed", "#0891b2", "#db2777", "#4f46e5"];

  interface FleetData {
    aligned: uPlot.AlignedData;
    dates: number[];
  }

  function fleetData(): FleetData {
    const all = tracks.flatMap((track) => track.points);
    const keys = new Map<string, { ms: number; hash: string }>();
    for (const point of all) {
      const key = axis === "commit" ? `${point.commitHash}\u0000${point.chartMs}` : String(point.chartMs);
      keys.set(key, { ms: point.chartMs, hash: point.commitHash });
    }
    const ordered = [...keys.entries()].sort((a, b) => a[1].ms - b[1].ms || a[0].localeCompare(b[0]));
    const index = new Map(ordered.map(([key], i) => [key, i]));
    const xs = ordered.map(([, value], i) => axis === "time" ? value.ms / 1000 : i);
    const values = tracks.map((track) => {
      const ys: (number | null)[] = Array(ordered.length).fill(null);
      for (const point of track.points) {
        const key = axis === "commit" ? `${point.commitHash}\u0000${point.chartMs}` : String(point.chartMs);
        const i = index.get(key);
        if (i !== undefined) ys[i] = point.svs;
      }
      return ys;
    });
    return { aligned: [xs, ...values] as uPlot.AlignedData, dates: ordered.map(([, value]) => value.ms) };
  }

  function cssVar(name: string, fallback: string): string {
    const value = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    return value === "" ? fallback : value;
  }

  function shortDate(ms: number): string {
    return new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" }).format(new Date(ms));
  }

  function options(width: number, data: FleetData): uPlot.Options {
    const axisColor = cssVar("--c-text-muted", "#57606a");
    const gridColor = cssVar("--c-border-muted", "#e4e6ec");
    const range = paddedValueRange(tracks.flatMap((track) => track.points.map((point) => point.svs)));
    return {
      width,
      height,
      legend: { show: true, live: true },
      scales: {
        x: { time: axis === "time" },
        y: range === null ? {} : { range: () => [range.min, range.max] },
      },
      series: [
        {},
        ...tracks.map((track, i) => ({
          label: track.machineName,
          stroke: palette[i % palette.length]!,
          width: 2,
          spanGaps: true,
          points: { show: true, size: 6 },
          value: (_u: uPlot, value: number | null) => value == null ? "—" : compactAxisValue(value),
        })),
      ],
      axes: [
        axis === "commit"
          ? {
              stroke: axisColor,
              grid: { stroke: gridColor, width: 1 },
              values: (_u, ticks) => ticks.map((tick) => Number.isInteger(tick) && data.dates[tick] !== undefined ? shortDate(data.dates[tick]!) : ""),
            }
          : { stroke: axisColor, grid: { stroke: gridColor, width: 1 } },
        {
          size: 76,
          stroke: axisColor,
          grid: { stroke: gridColor, width: 1 },
          values: (_u, ticks) => ticks.map((tick) => compactAxisValue(Number(tick))),
        },
      ],
    };
  }

  $effect(() => {
    const data = fleetData();
    void height;
    void resolvedTheme();
    chart?.destroy();
    chart = untrack(() => new uPlot(options(host.clientWidth || 640, data), data.aligned, host));
  });

  $effect(() => {
    if (!host || typeof ResizeObserver === "undefined") return;
    resizeObserver?.disconnect();
    resizeObserver = new ResizeObserver(() => chart?.setSize({ width: host.clientWidth || 640, height }));
    resizeObserver.observe(host);
    return () => resizeObserver?.disconnect();
  });

  onDestroy(() => {
    resizeObserver?.disconnect();
    chart?.destroy();
  });
</script>

<div class="fleet-chart" aria-label="Fleet benchmark trend">
  <div bind:this={host}></div>
</div>

<style>
  .fleet-chart {
    min-width: 0;
    padding: 10px;
    border: 1px solid var(--c-border-muted);
    border-radius: var(--radius-md);
    background: var(--c-chart-bg);
  }
  .fleet-chart :global(.uplot) { width: 100% !important; }
  .fleet-chart :global(.u-legend) {
    display: flex;
    flex-wrap: wrap;
    gap: 8px 14px;
    margin-bottom: 6px;
    color: var(--c-text-muted);
    font-size: 0.76rem;
  }
  .fleet-chart :global(.u-legend .u-series) { display: inline-flex; gap: 5px; align-items: center; }
  .fleet-chart :global(.u-legend .u-label) { font-weight: 700; }
  .fleet-chart :global(.u-legend .u-value) { font-variant-numeric: tabular-nums; }
</style>
