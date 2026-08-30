<script lang="ts">
  import uPlot from "uplot";
  import "uplot/dist/uPlot.min.css";
  import { onDestroy, tick, untrack } from "svelte";

  import {
    closestIndexForValue,
    clampRangeToDomain,
    tooltipLeftForCursor,
    tooltipTopForCursor,
    type ValueRange,
    observedValueRange,
    zeroBasedValueRange,
  } from "../series/chart-geometry";
  import {
    pointTooltip,
    segmentSpans,
    stepIndices,
    trendChartData,
    trendYRangeValues,
    type SeriesPoint,
    type TrendTooltip,
  } from "../series/transform";
  import { compactAxisValue } from "../series/chart-format";
  import { formatMeasurement } from "../format";
  import { resolvedTheme } from "../theme.svelte";

  let {
    points,
    sigma = 2,
    zeroBased = true,
    height = 280,
    selectedIndex = null,
    currentResultId = null,
    markedIndices = [],
    onselect,
    onopen,
  }: {
    points: SeriesPoint[];
    sigma?: number;
    zeroBased?: boolean;
    height?: number;
    selectedIndex?: number | null;
    currentResultId?: string | null;
    markedIndices?: number[];
    onselect?: (index: number) => void;
    onopen?: (resultId: string) => void;
  } = $props();

  let chartWrap: HTMLDivElement;
  let plotHost: HTMLDivElement;
  let host: HTMLDivElement;
  let tooltipElement = $state<HTMLDivElement>();
  let tooltipRequestID = 0;
  let chart: uPlot | undefined;

  let tip = $state<{
    left: number;
    top: number;
    positioned: boolean;
    requestID: number;
    vm: TrendTooltip;
  } | null>(null);
  let hoverIndex = $state<number | null>(null);
  let resizeObserver: ResizeObserver | undefined;
  let plotBox = $state({ left: 0, top: 0, width: 0, height: 0 });
  let zoomWindow = $state<{ min: number; max: number } | null>(null);

  /** cssVar resolves a design token for canvas drawing (canvas cannot read CSS
   * custom properties); the fallback keeps jsdom and detached nodes working. */
  function cssVar(name: string, fallback: string): string {
    const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    return v === "" ? fallback : v;
  }

  function drawSegments(u: uPlot): void {
    const shade = cssVar("--c-segment-shade", "rgba(148, 163, 184, 0.1)");
    const xs = u.data[0]!;
    u.ctx.save();
    u.ctx.fillStyle = shade;
    for (const span of segmentSpans(points)) {
      if (span.segmentId % 2 === 0) continue;
      const x0 = u.valToPos(xs[span.startIndex]!, "x", true);
      const x1 = u.valToPos(xs[span.endIndex]!, "x", true);
      u.ctx.fillRect(x0, u.bbox.top, Math.max(x1 - x0, 1), u.bbox.height);
    }
    u.ctx.restore();
  }

  function drawSteps(u: uPlot): void {
    const color = cssVar("--c-step-marker", "#f59e0b");
    const xs = u.data[0]!;
    u.ctx.save();
    u.ctx.strokeStyle = color;
    u.ctx.lineWidth = 1.5;
    u.ctx.setLineDash([5, 4]);
    for (const i of stepIndices(points)) {
      const x = u.valToPos(xs[i]!, "x", true);
      u.ctx.beginPath();
      u.ctx.moveTo(x, u.bbox.top);
      u.ctx.lineTo(x, u.bbox.top + u.bbox.height);
      u.ctx.stroke();
    }
    u.ctx.restore();
  }

  /** drawMarked rings the compare baseline/contender points. Violet
   * (--c-trend-mean) distinguishes them from the blue selection ring. */
  function drawMarked(u: uPlot): void {
    if (markedIndices.length === 0) return;
    const color = cssVar("--c-trend-mean", "#8b5cf6");
    const xs = u.data[0]!;
    u.ctx.save();
    u.ctx.strokeStyle = color;
    u.ctx.lineWidth = 2;
    for (const i of markedIndices) {
      const p = points[i];
      const x = xs[i];
      if (!p || x == null) continue;
      u.ctx.beginPath();
      u.ctx.arc(u.valToPos(x, "x", true), u.valToPos(p.svs, "y", true), 6, 0, 2 * Math.PI);
      u.ctx.stroke();
    }
    u.ctx.restore();
  }

  function drawSelection(u: uPlot): void {
    if (selectedIndex == null) return;
    const p = points[selectedIndex];
    const x = u.data[0]![selectedIndex];
    if (!p || x == null) return;
    u.ctx.save();
    u.ctx.beginPath();
    u.ctx.arc(u.valToPos(x, "x", true), u.valToPos(p.svs, "y", true), 6, 0, 2 * Math.PI);
    u.ctx.strokeStyle = cssVar("--c-accent", "#3b82f6");
    u.ctx.lineWidth = 2;
    u.ctx.stroke();
    u.ctx.restore();
  }

  function syncPlotBox() {
    if (!host || !plotHost) return;
    const over = host.querySelector(".u-over");
    if (!(over instanceof HTMLElement)) return;
    const plotRect = plotHost.getBoundingClientRect();
    const overRect = over.getBoundingClientRect();
    plotBox = {
      left: overRect.left - plotRect.left,
      top: overRect.top - plotRect.top,
      width: overRect.width,
      height: overRect.height,
    };
  }

  function pathPoint(x: number, y: number): string {
    return `${x.toFixed(2)},${y.toFixed(2)}`;
  }

  // Cache the Y range so hover movement does not re-scan the whole
  // history on every cursor change; it only recomputes when points/sigma change.
  let overlayRange = $derived(
    (zeroBased ? zeroBasedValueRange : observedValueRange)(trendYRangeValues(points, sigma)),
  );

  function overlayX(point: SeriesPoint): number {
    if (points.length === 1) return plotBox.width / 2;
    const first = zoomWindow?.min === undefined
      ? (points[0]?.chartMs ?? point.chartMs)
      : zoomWindow.min * 1000;
    const last = zoomWindow?.max === undefined
      ? (points[points.length - 1]?.chartMs ?? point.chartMs)
      : zoomWindow.max * 1000;
    const span = last - first || 1;
    return ((point.chartMs - first) / span) * plotBox.width;
  }

  function overlayY(value: number, range: ValueRange): number {
    const span = range.max - range.min || 1;
    return ((range.max - value) / span) * plotBox.height;
  }

  function svsPath(): string {
    const { width, height } = plotBox;
    if (points.length < 2 || width <= 0 || height <= 0) {
      return "";
    }
    const range = overlayRange;
    if (range === null) return "";
    return points
      .map((p, i) => {
        const x = overlayX(p);
        const y = overlayY(p.svs, range);
        return `${i === 0 ? "M" : "L"}${pathPoint(x, y)}`;
      })
      .join(" ");
  }

  function bandPaths(): string[] {
    const { width, height } = plotBox;
    if (points.length < 2 || width <= 0 || height <= 0) {
      return [];
    }
    const range = overlayRange;
    if (range === null) return [];
    const paths: string[] = [];
    let upper: string[] = [];
    let lower: string[] = [];

    function flush() {
      if (upper.length < 2) {
        upper = [];
        lower = [];
        return;
      }
      paths.push(`M${upper.join(" L")} L${lower.reverse().join(" L")} Z`);
      upper = [];
      lower = [];
    }

    for (let i = 0; i < points.length; i++) {
      const p = points[i]!;
      if (p.stats.rollingMean === null || p.stats.rollingStddev === null) {
        flush();
        continue;
      }
      const x = overlayX(p);
      const hi = p.stats.rollingMean + sigma * p.stats.rollingStddev;
      const lo = p.stats.rollingMean - sigma * p.stats.rollingStddev;
      upper.push(pathPoint(x, overlayY(hi, range)));
      lower.push(pathPoint(x, overlayY(lo, range)));
    }
    flush();
    return paths;
  }

  function hoverPointPosition(): { x: number; y: number } | null {
    const i = hoverIndex;
    if (i == null) return null;
    const p = points[i];
    if (!p || plotBox.width <= 0 || plotBox.height <= 0) return null;
    const range = overlayRange;
    if (range === null) return null;
    return {
      x: overlayX(p),
      y: overlayY(p.svs, range),
    };
  }

  interface OverlayPoint {
    key: string;
    x: number;
    y: number;
    current: boolean;
    outlier: boolean;
  }

  function rawOverlayPoints(): OverlayPoint[] {
    const { width, height } = plotBox;
    if (width <= 0 || height <= 0) return [];
    const range = overlayRange;
    if (range === null) return [];
    return points.flatMap((p) =>
      p.measurements.map((value, j) => ({
        key: `${p.resultId}-raw-${j}`,
        x: overlayX(p),
        y: overlayY(value, range),
        current: currentResultId !== null && p.resultId === currentResultId,
        outlier: false,
      })),
    );
  }

  function svsOverlayPoints(): OverlayPoint[] {
    const { width, height } = plotBox;
    if (width <= 0 || height <= 0) return [];
    const range = overlayRange;
    if (range === null) return [];
    return points.map((p) => ({
      key: p.resultId,
      x: overlayX(p),
      y: overlayY(p.svs, range),
      current: currentResultId !== null && p.resultId === currentResultId,
      outlier: p.stats.isOutlier,
    }));
  }

  function currentPointPosition(): { x: number; y: number } | null {
    const i = currentIndex;
    if (i == null) return null;
    const p = points[i];
    if (!p || plotBox.width <= 0 || plotBox.height <= 0) return null;
    const range = overlayRange;
    if (range === null) return null;
    return {
      x: overlayX(p),
      y: overlayY(p.svs, range),
    };
  }

  let svsOverlayPath = $derived(svsPath());
  let sigmaBandPaths = $derived(bandPaths());
  let rawPoints = $derived(rawOverlayPoints());
  let svsPoints = $derived(svsOverlayPoints());
  let currentIndex = $derived(
    currentResultId === null
      ? null
      : (() => {
          const index = points.findIndex((p) => p.resultId === currentResultId);
          return index < 0 ? null : index;
        })(),
  );
  let currentPoint = $derived(currentPointPosition());
  let hoverPoint = $derived(hoverPointPosition());

  function hoveredIndex(u: uPlot): number | null {
    const left = u.cursor.left;
    if (left == null || left < 0 || points.length === 0) return null;
    const min = zoomWindow?.min ?? points[0]!.chartMs / 1000;
    const max = zoomWindow?.max ?? points[points.length - 1]!.chartMs / 1000;
    const dpr = window.devicePixelRatio || 1;
    const width = u.bbox.width / dpr;
    if (width <= 0) return null;
    const fraction = Math.min(1, Math.max(0, left / width));
    const seconds = min + (max - min) * fraction;
    return closestIndexForValue(seconds * 1000, points.map((p) => p.chartMs));
  }

  function plotOffset() {
    if (!chartWrap || !plotHost) return { left: 0, top: 0 };
    const wrapRect = chartWrap.getBoundingClientRect();
    const plotRect = plotHost.getBoundingClientRect();
    return {
      left: plotRect.left - wrapRect.left,
      top: plotRect.top - wrapRect.top,
    };
  }

  async function showTooltip(p: SeriesPoint, cursorLeft: number, cursorTop: number) {
    const vm = pointTooltip(p);
    const requestID = ++tooltipRequestID;
    tip = {
      left: tooltipLeftForCursor(cursorLeft, chartWrap?.clientWidth ?? 0),
      top: 8,
      positioned: false,
      requestID,
      vm,
    };
    await tick();
    if (tip?.requestID !== requestID || !tooltipElement) return;
    const measuredHeight = tooltipElement.getBoundingClientRect().height;
    tip = {
      ...tip,
      top: tooltipTopForCursor(
        cursorTop,
        chartWrap?.clientHeight ?? 0,
        measuredHeight,
      ).top,
      positioned: true,
    };
  }

  function options(width: number): uPlot.Options {
    const accent = cssVar("--c-accent", "#3b82f6");
    const meanColor = cssVar("--c-trend-mean", "#8b5cf6");
    const axisColor = cssVar("--c-text-muted", "#57606a");
    const gridColor = cssVar("--c-border-muted", "#e4e6ec");
    const yRange = overlayRange;
    const xAxis: uPlot.Axis = {
      stroke: axisColor,
      grid: { stroke: gridColor, width: 1 },
    };
    const xScale: uPlot.Scale = { time: true };
    if (zoomWindow !== null) {
      const { min, max } = zoomWindow;
      xScale.range = () => [min, max];
    }
    return {
      width,
      height,
      legend: { show: false },
      scales: {
        x: xScale,
        y: yRange === null ? {} : { range: () => [yRange.min, yRange.max] },
      },
      cursor: { drag: { x: true, y: false, dist: 8, setScale: true } },
      series: [
        {},
        { label: "result value", stroke: accent, width: 2, points: { show: false } },
        { label: "rolling mean", stroke: meanColor, width: 1.5, dash: [6, 4], points: { show: false } },
        { label: "hi", stroke: "transparent", points: { show: false } },
        { label: "lo", stroke: "transparent", points: { show: false } },
      ],
      axes: [
        xAxis,
        {
          size: 76,
          stroke: axisColor,
          grid: { stroke: gridColor, width: 1 },
          values: (_u, ticks) => ticks.map((t) =>
            points[0]?.unit === "B"
              ? formatMeasurement(Number(t), "B")
              : compactAxisValue(Number(t)),
          ),
        },
      ],
      hooks: {
        ready: [() => requestAnimationFrame(syncPlotBox)],
        setSelect: [rememberZoom],
        drawClear: [drawSegments],
        draw: [drawSteps, drawMarked, drawSelection],
        setCursor: [
          (u) => {
            const i = hoveredIndex(u);
            const p = i == null ? undefined : points[i];
            const dpr = window.devicePixelRatio || 1;
            const offset = plotOffset();
            const cursorLeft = offset.left + (u.cursor.left ?? 0) + u.bbox.left / dpr;
            const top = offset.top + (u.cursor.top ?? 0) + u.bbox.top / dpr;
            hoverIndex = i;
            if (p) {
              void showTooltip(p, cursorLeft, top);
            } else {
              tip = null;
            }
          },
        ],
      },
    };
  }

  // Rebuild on data/sigma changes; series are small, so rebuilds are cheap.
  // The constructor is untracked so reads inside uPlot-invoked hooks (the draw
  // closures read selectedIndex) can never become dependencies of this effect —
  // selection changes must only trigger the redraw effect below, not a rebuild.
  $effect(() => {
    const data = trendChartData(points, sigma);
    const domain = points.length < 2
      ? null
      : {
          min: points[0]!.chartMs / 1000,
          max: points[points.length - 1]!.chartMs / 1000,
        };
    const currentZoom = untrack(() => zoomWindow);
    const nextZoom = clampRangeToDomain(currentZoom, domain);
    if (nextZoom !== currentZoom) zoomWindow = nextZoom;
    // height is otherwise only read inside the untracked constructor; track it
    // here so a caller-driven height change rebuilds instead of being ignored.
    void height;
    void zeroBased;
    // Canvas colors are read from CSS tokens at build time, so a theme switch
    // must rebuild the chart to pick up the new palette.
    void resolvedTheme();
    chart?.destroy();
    chart = untrack(
      () => new uPlot(options(host.clientWidth || 640), data as uPlot.AlignedData, host),
    );
    requestAnimationFrame(syncPlotBox);
  });

  $effect(() => {
    if (!host || typeof ResizeObserver === "undefined") return;
    resizeObserver?.disconnect();
    resizeObserver = new ResizeObserver(() => {
      const width = host.clientWidth || 640;
      chart?.setSize({ width, height });
      requestAnimationFrame(syncPlotBox);
    });
    resizeObserver.observe(host);
    return () => {
      resizeObserver?.disconnect();
      resizeObserver = undefined;
    };
  });

  onDestroy(() => {
    resizeObserver?.disconnect();
    chart?.destroy();
  });

  $effect(() => {
    // Re-run the draw hooks (which read selectedIndex and markedIndices) when
    // the table changes the selection or the compare marks change, so the
    // chart marks the same points.
    void selectedIndex;
    void currentResultId;
    void markedIndices;
    chart?.redraw();
  });

  function selectAtCursor() {
    if (hoverIndex != null) {
      onselect?.(hoverIndex);
      onopen?.(points[hoverIndex]!.resultId);
    }
  }

  function rememberZoom(u: uPlot) {
    if (u.select.width < 8) return;
    const left = u.posToVal(u.select.left, "x");
    const right = u.posToVal(u.select.left + u.select.width, "x");
    if (!Number.isFinite(left) || !Number.isFinite(right) || left === right) return;
    zoomWindow = { min: Math.min(left, right), max: Math.max(left, right) };
    hoverIndex = null;
    tip = null;
  }

  function resetZoom(event: MouseEvent) {
    event.stopPropagation();
    hoverIndex = null;
    tip = null;
    if (chart === undefined || points.length === 0) return;
    zoomWindow = null;
    chart.setScale("x", {
      min: points[0]!.chartMs / 1000,
      max: points[points.length - 1]!.chartMs / 1000,
    });
  }
</script>

<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
<!-- The chart is a pointer-driven visualization; keyboard-accessible selection and
     navigation are provided by the DetailTable rows/links that share state. -->
<div class="chart-wrap" class:clickable={onopen !== undefined} bind:this={chartWrap} onclick={selectAtCursor}>
  <div class="chart-heading">
    <div class="legend" aria-hidden="true">
      <span><i class="svs"></i>result value</span>
      <span><i class="raw"></i>repetitions</span>
      <span><i class="inlier"></i>inlier</span>
      <span><i class="outlier"></i>outlier</span>
      {#if currentResultId !== null}<span><i class="current"></i>current</span>{/if}
      <span><i class="mean"></i>rolling mean</span>
      <span><i class="band"></i>{sigma}σ band</span>
    </div>
    <div class="zoom-controls">
      {#if zoomWindow === null}
        <span>Drag horizontally to zoom</span>
      {:else}
        <span>Zoomed</span>
        <button type="button" onclick={resetZoom}>Reset zoom</button>
      {/if}
    </div>
  </div>
  <div class="plot-host" bind:this={plotHost}>
    <div bind:this={host}></div>
    {#if svsOverlayPath !== "" || sigmaBandPaths.length > 0 || rawPoints.length > 0 || svsPoints.length > 0 || currentPoint || hoverPoint}
      <svg
        class="chart-overlay"
        style={`left:${plotBox.left}px;top:${plotBox.top}px;width:${plotBox.width}px;height:${plotBox.height}px`}
        viewBox={`0 0 ${plotBox.width} ${plotBox.height}`}
        aria-hidden="true"
      >
        {#each sigmaBandPaths as path}
          <path class="band-fill" d={path} />
        {/each}
        {#if svsOverlayPath !== ""}
          <path class="svs-line" d={svsOverlayPath} />
        {/if}
        {#each rawPoints as point (point.key)}
          <circle
            class:current-raw={point.current}
            class="raw-point"
            cx={point.x}
            cy={point.y}
            r={point.current ? 2.5 : 1.8}
          />
        {/each}
        {#each svsPoints as point (point.key)}
          <circle
            class:outlier={point.outlier}
            class:inlier={!point.outlier}
            class="svs-point"
            cx={point.x}
            cy={point.y}
            r={point.outlier ? 4.3 : 3}
          />
        {/each}
        {#if currentPoint}
          <g class="current-marker">
            <line x1={currentPoint.x - 8} y1={currentPoint.y - 8} x2={currentPoint.x + 8} y2={currentPoint.y + 8} />
            <line x1={currentPoint.x + 8} y1={currentPoint.y - 8} x2={currentPoint.x - 8} y2={currentPoint.y + 8} />
          </g>
        {/if}
        {#if hoverPoint}
          <circle class="hover-point" cx={hoverPoint.x} cy={hoverPoint.y} r="5" />
        {/if}
      </svg>
    {/if}
  </div>
  {#if tip}
    <div
      class="tip"
      bind:this={tooltipElement}
      style={`left:${tip.left}px;top:${tip.top}px;visibility:${tip.positioned ? "visible" : "hidden"}`}
    >
      <strong>{tip.vm.title}</strong>
      {#each tip.vm.lines as line, i (i)}
        <div>{line}</div>
      {/each}
      {#each tip.vm.metadata as line, i (i)}
        <div class="tip-metadata">{line}</div>
      {/each}
      {#if onopen !== undefined}<div class="tip-action">click to open result</div>{/if}
    </div>
  {/if}
</div>

<style>
  .chart-wrap {
    position: relative;
    padding: 10px 10px 6px;
    border: 1px solid var(--c-border-muted);
    border-radius: var(--radius-md);
    background: var(--c-chart-bg);
    min-width: 0;
  }
  .chart-wrap.clickable { cursor: pointer; }
  .chart-wrap :global(.uplot) {
    width: 100% !important;
  }
  .chart-wrap :global(.u-over) { cursor: crosshair; }
  .plot-host {
    position: relative;
  }
  .chart-heading {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    justify-content: space-between;
    gap: 12px;
    margin-bottom: 6px;
  }
  .zoom-controls {
    display: flex;
    align-items: center;
    gap: 7px;
    flex: 0 0 auto;
    color: var(--c-text-muted);
    font-size: 0.72rem;
  }
  .zoom-controls button {
    padding: 3px 7px;
    border: 1px solid var(--c-border-muted);
    border-radius: var(--radius-sm);
    background: var(--c-surface);
    color: var(--c-text);
    cursor: pointer;
  }
  .legend {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 12px;
    color: var(--c-text-muted);
    font-size: 0.72rem;
  }
  .legend span {
    display: inline-flex;
    align-items: center;
    gap: 5px;
  }
  .legend i {
    width: 14px;
    height: 2px;
    border-radius: 999px;
    display: inline-block;
  }
  .legend .svs {
    background: var(--c-accent);
  }
  .legend .raw {
    width: 7px;
    height: 7px;
    background: var(--c-raw-point);
    border-radius: 50%;
  }
  .legend .inlier {
    width: 7px;
    height: 7px;
    background: var(--c-chart-point);
    border-radius: 50%;
  }
  .legend .outlier {
    width: 7px;
    height: 7px;
    background: var(--c-chart-bg);
    border: 1.5px solid var(--c-warning);
    border-radius: 50%;
  }
  .legend .current {
    position: relative;
    width: 10px;
    height: 10px;
  }
  .legend .current::before,
  .legend .current::after {
    content: "";
    position: absolute;
    left: 0;
    top: 4px;
    width: 10px;
    height: 2px;
    background: var(--c-current-result);
    border-radius: 999px;
  }
  .legend .current::before {
    transform: rotate(45deg);
  }
  .legend .current::after {
    transform: rotate(-45deg);
  }
  .legend .mean {
    background: var(--c-trend-mean);
  }
  .legend .band {
    height: 8px;
    background: var(--c-band-fill);
    border: 1px solid color-mix(in srgb, var(--c-accent) 25%, transparent);
  }
  .chart-overlay {
    position: absolute;
    pointer-events: none;
    overflow: visible;
    z-index: 2;
  }
  .chart-overlay .band-fill {
    fill: var(--c-band-fill);
    stroke: color-mix(in srgb, var(--c-accent) 18%, transparent);
    stroke-width: 1;
  }
  .chart-overlay .svs-line {
    fill: none;
    stroke: var(--c-accent);
    stroke-width: 2;
    stroke-linejoin: round;
    stroke-linecap: round;
  }
  .chart-overlay .raw-point {
    fill: var(--c-raw-point);
    opacity: 0.68;
    vector-effect: non-scaling-stroke;
  }
  .chart-overlay .raw-point.current-raw {
    fill: var(--c-current-result);
    opacity: 0.72;
  }
  .chart-overlay .svs-point.inlier {
    fill: var(--c-chart-point);
    stroke: color-mix(in srgb, var(--c-chart-bg) 45%, transparent);
    stroke-width: 0.7;
    vector-effect: non-scaling-stroke;
  }
  .chart-overlay .svs-point.outlier {
    fill: var(--c-chart-bg);
    stroke: var(--c-warning);
    stroke-width: 2;
    vector-effect: non-scaling-stroke;
  }
  .chart-overlay .current-marker {
    stroke: var(--c-current-result);
    stroke-width: 2.5;
    stroke-linecap: round;
    vector-effect: non-scaling-stroke;
  }
  .chart-overlay .hover-point {
    fill: var(--c-chart-bg);
    stroke: var(--c-accent);
    stroke-width: 2.5;
    vector-effect: non-scaling-stroke;
  }
  .tip {
    position: absolute;
    background: #181b24;
    color: #fff;
    font-size: 0.72rem;
    line-height: 1.3;
    padding: 0.25rem 0.5rem;
    border-radius: 4px;
    pointer-events: none;
    width: max-content;
    max-width: min(28rem, calc(100% - 16px));
    max-height: calc(100% - 16px);
    overflow-y: auto;
    white-space: normal;
    overflow-wrap: anywhere;
    z-index: 3;
  }
  .tip-action { margin-top: 0.2rem; color: #cbd5e1; font-weight: 650; }
</style>
