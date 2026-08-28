<script lang="ts">
  import uPlot from "uplot";
  import "uplot/dist/uPlot.min.css";
  import { onDestroy, tick, untrack } from "svelte";

  import { formatMeasurement } from "../format";
  import { compactAxisValue } from "../series/chart-format";
  import {
    tooltipLeftForCursor,
    tooltipTopForCursor,
    zeroBasedValueRange,
  } from "../series/chart-geometry";
  import type { MachineTrack } from "../series/loader";
  import { pointTooltip, type SeriesPoint, type TrendTooltip } from "../series/transform";
  import { resolvedTheme } from "../theme.svelte";

  let {
    tracks,
    sigma = 2,
    height = 320,
    onopen,
  }: {
    tracks: MachineTrack[];
    sigma?: number;
    height?: number;
    onopen?: (resultId: string) => void;
  } = $props();

  let chartWrap: HTMLDivElement;
  let host: HTMLDivElement;
  let tooltipElement = $state<HTMLDivElement>();
  let tooltipRequestID = 0;
  let chart: uPlot | undefined;
  let resizeObserver: ResizeObserver | undefined;
  let hovered = $state<{ point: SeriesPoint; machineName: string } | null>(null);
  let tip = $state<{
    left: number;
    top: number;
    positioned: boolean;
    requestID: number;
    machineName: string;
    vm: TrendTooltip;
  } | null>(null);

  const palette = ["#2563eb", "#dc2626", "#059669", "#d97706", "#7c3aed", "#0891b2", "#db2777", "#4f46e5"];

  interface FleetData {
    aligned: uPlot.AlignedData;
    points: (SeriesPoint | null)[][];
  }

  function pointKey(point: SeriesPoint): string {
    return `${point.commitHash}\u0000${point.chartMs}`;
  }

  function fleetData(): FleetData {
    const keys = new Map<string, { ms: number; hash: string }>();
    for (const point of tracks.flatMap((track) => track.points)) {
      keys.set(pointKey(point), { ms: point.chartMs, hash: point.commitHash });
    }
    const ordered = [...keys.entries()].sort((a, b) => a[1].ms - b[1].ms || a[0].localeCompare(b[0]));
    const index = new Map(ordered.map(([key], i) => [key, i]));
    const xs = ordered.map(([, value]) => value.ms / 1000);
    const pointRows: (SeriesPoint | null)[][] = [];
    const values: (number | null)[][] = [];

    for (const track of tracks) {
      const points: (SeriesPoint | null)[] = Array(ordered.length).fill(null);
      for (const point of track.points) {
        const i = index.get(pointKey(point));
        if (i !== undefined) points[i] = point;
      }
      pointRows.push(points);
      values.push(
        points.map((point) => point?.svs ?? null),
        points.map((point) => point?.stats.rollingMean ?? null),
        points.map((point) =>
          point?.stats.rollingMean != null && point.stats.rollingStddev != null
            ? point.stats.rollingMean + sigma * point.stats.rollingStddev
            : null,
        ),
        points.map((point) =>
          point?.stats.rollingMean != null && point.stats.rollingStddev != null
            ? point.stats.rollingMean - sigma * point.stats.rollingStddev
            : null,
        ),
      );
    }
    return {
      aligned: [xs, ...values] as uPlot.AlignedData,
      points: pointRows,
    };
  }

  function cssVar(name: string, fallback: string): string {
    const value = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    return value === "" ? fallback : value;
  }

  function closestPoint(u: uPlot, data: FleetData): { point: SeriesPoint; machineName: string } | null {
    const idx = u.cursor.idx;
    if (idx == null || idx < 0) return null;
    const candidates = data.points.flatMap((points, trackIndex) => {
      const point = points[idx];
      return point === null || point === undefined
        ? []
        : [{ point, machineName: tracks[trackIndex]!.machineName }];
    });
    if (candidates.length <= 1 || u.cursor.top == null) return candidates[0] ?? null;
    const cursorValue = u.posToVal(u.cursor.top, "y");
    return candidates.reduce((best, candidate) =>
      Math.abs(candidate.point.svs - cursorValue) < Math.abs(best.point.svs - cursorValue)
        ? candidate
        : best,
    );
  }

  function chartOffset() {
    if (!chartWrap || !host) return { left: 0, top: 0 };
    const wrapRect = chartWrap.getBoundingClientRect();
    const hostRect = host.getBoundingClientRect();
    return { left: hostRect.left - wrapRect.left, top: hostRect.top - wrapRect.top };
  }

  async function showTooltip(
    target: { point: SeriesPoint; machineName: string },
    cursorLeft: number,
    cursorTop: number,
  ) {
    const requestID = ++tooltipRequestID;
    tip = {
      left: tooltipLeftForCursor(cursorLeft, chartWrap?.clientWidth ?? 0),
      top: 8,
      positioned: false,
      requestID,
      machineName: target.machineName,
      vm: pointTooltip(target.point),
    };
    await tick();
    if (tip?.requestID !== requestID || !tooltipElement) return;
    tip = {
      ...tip,
      top: tooltipTopForCursor(
        cursorTop,
        chartWrap?.clientHeight ?? 0,
        tooltipElement.getBoundingClientRect().height,
      ).top,
      positioned: true,
    };
  }

  function options(width: number, data: FleetData): uPlot.Options {
    const axisColor = cssVar("--c-text-muted", "#57606a");
    const gridColor = cssVar("--c-border-muted", "#e4e6ec");
    const range = zeroBasedValueRange(tracks.flatMap((track) => track.points.flatMap((point) => {
      const values = [point.svs];
      if (point.stats.rollingMean !== null) {
        values.push(point.stats.rollingMean);
        if (point.stats.rollingStddev !== null) {
          values.push(point.stats.rollingMean + sigma * point.stats.rollingStddev);
          values.push(point.stats.rollingMean - sigma * point.stats.rollingStddev);
        }
      }
      return values;
    })));
    const series: uPlot.Series[] = [{}];
    const bands: uPlot.Band[] = [];
    tracks.forEach((track, i) => {
      const color = palette[i % palette.length]!;
      const valueIndex = 1 + i * 4;
      series.push(
        { label: track.machineName, stroke: color, width: 2, spanGaps: true, points: { show: true, size: 6 } },
        { label: `${track.machineName} rolling mean`, stroke: color, width: 1.5, dash: [6, 4], points: { show: false } },
        { label: `${track.machineName} high`, stroke: "transparent", points: { show: false } },
        { label: `${track.machineName} low`, stroke: "transparent", points: { show: false } },
      );
      bands.push({ series: [valueIndex + 2, valueIndex + 3], fill: `${color}20` });
    });
    const unit = tracks[0]?.points[0]?.unit ?? null;
    return {
      width,
      height,
      legend: { show: false },
      scales: {
        x: { time: true },
        y: range === null ? {} : { range: () => [range.min, range.max] },
      },
      series,
      bands,
      axes: [
        { stroke: axisColor, grid: { stroke: gridColor, width: 1 } },
        {
          size: unit === "B" ? 92 : 76,
          stroke: axisColor,
          grid: { stroke: gridColor, width: 1 },
          values: (_u, ticks) => ticks.map((tick) =>
            unit === "B" ? formatMeasurement(Number(tick), "B") : compactAxisValue(Number(tick)),
          ),
        },
      ],
      hooks: {
        setCursor: [
          (u) => {
            const target = closestPoint(u, data);
            hovered = target;
            if (target === null) {
              tip = null;
              return;
            }
            const dpr = window.devicePixelRatio || 1;
            const offset = chartOffset();
            void showTooltip(
              target,
              offset.left + (u.cursor.left ?? 0) + u.bbox.left / dpr,
              offset.top + (u.cursor.top ?? 0) + u.bbox.top / dpr,
            );
          },
        ],
      },
    };
  }

  $effect(() => {
    const data = fleetData();
    void height;
    void sigma;
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

  function openHoveredPoint() {
    if (hovered !== null) onopen?.(hovered.point.resultId);
  }

  function trackSummary(track: MachineTrack): string {
    const latest = track.points[track.points.length - 1];
    const count = `${track.points.length} ${track.points.length === 1 ? "result" : "results"}`;
    return latest === undefined ? count : `${count} · ${formatMeasurement(latest.svs, latest.unit)}`;
  }
</script>

<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
<div class="fleet-chart" aria-label="Fleet benchmark trend" bind:this={chartWrap} onclick={openHoveredPoint}>
  <div class="legend" aria-hidden="true">
    {#each tracks as track, i (track.machineName)}
      <span><i class="machine" style={`background:${palette[i % palette.length]}`}></i><strong>{track.machineName}</strong> · {trackSummary(track)}</span>
    {/each}
    <span><i class="mean"></i>rolling mean</span>
    <span><i class="band"></i>{sigma}σ range</span>
  </div>
  <div bind:this={host}></div>
  {#if tip}
    <div
      class="tip"
      bind:this={tooltipElement}
      style={`left:${tip.left}px;top:${tip.top}px;visibility:${tip.positioned ? "visible" : "hidden"}`}
    >
      <strong>{tip.machineName}</strong>
      <div>{tip.vm.title}</div>
      {#each tip.vm.lines as line, i (i)}<div>{line}</div>{/each}
      {#each tip.vm.metadata as line, i (i)}<div class="tip-metadata">{line}</div>{/each}
      <div class="tip-action">click to open result</div>
    </div>
  {/if}
</div>

<style>
  .fleet-chart {
    position: relative;
    min-width: 0;
    padding: 10px;
    border: 1px solid var(--c-border-muted);
    border-radius: var(--radius-md);
    background: var(--c-chart-bg);
    cursor: pointer;
  }
  .fleet-chart :global(.uplot) { width: 100% !important; }
  .legend {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px 14px;
    margin-bottom: 6px;
    color: var(--c-text-muted);
    font-size: 0.72rem;
  }
  .legend span { display: inline-flex; align-items: center; gap: 5px; }
  .legend strong { color: var(--c-text); font-weight: 650; }
  .legend i { display: inline-block; width: 14px; height: 2px; border-radius: 999px; }
  .legend .mean { border-top: 2px dashed var(--c-text-muted); height: 0; }
  .legend .band { height: 8px; background: color-mix(in srgb, var(--c-accent) 18%, transparent); }
  .tip {
    position: absolute;
    z-index: 6;
    width: min(448px, calc(100% - 16px));
    padding: 8px 10px;
    border: 1px solid var(--c-border);
    border-radius: var(--radius-sm);
    background: var(--c-surface);
    box-shadow: var(--shadow-panel);
    color: var(--c-text);
    font-size: 0.78rem;
    pointer-events: none;
  }
  .tip strong { display: block; margin-bottom: 2px; }
  .tip-metadata, .tip-action { color: var(--c-text-muted); }
  .tip-action { margin-top: 4px; font-weight: 650; }
</style>
