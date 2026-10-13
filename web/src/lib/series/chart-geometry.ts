export function indexForCursorValue(value: number, count: number): number | null {
  if (!Number.isFinite(value) || count <= 0) return null;
  const idx = Math.round(value);
  return Math.min(count - 1, Math.max(0, idx));
}

export function indexForCursorOffset(left: number, width: number, count: number): number | null {
  if (!Number.isFinite(left) || !Number.isFinite(width) || width <= 0 || count <= 0) return null;
  if (count === 1) return 0;
  const pct = Math.min(1, Math.max(0, left / width));
  return indexForCursorValue(pct * (count - 1), count);
}

export function closestIndexForValue(value: number, values: readonly number[]): number | null {
  if (!Number.isFinite(value) || values.length === 0) return null;
  let lo = 0;
  let hi = values.length - 1;
  while (lo < hi) {
    const mid = Math.floor((lo + hi) / 2);
    if (values[mid]! < value) {
      lo = mid + 1;
    } else {
      hi = mid;
    }
  }
  const right = lo;
  const left = Math.max(0, right - 1);
  return Math.abs(values[left]! - value) <= Math.abs(values[right]! - value) ? left : right;
}

export function closestIndexForSortedValueOffset(
  left: number,
  width: number,
  values: readonly number[],
): number | null {
  if (!Number.isFinite(left) || !Number.isFinite(width) || width <= 0 || values.length === 0) {
    return null;
  }
  const first = values[0]!;
  const last = values[values.length - 1]!;
  if (!Number.isFinite(first) || !Number.isFinite(last)) return null;
  const pct = Math.min(1, Math.max(0, left / width));
  return closestIndexForValue(first + (last - first) * pct, values);
}

export interface ValueRange {
  min: number;
  max: number;
}

export interface ComparisonMarker {
  role: "baseline" | "contender";
  resultId: string;
  chartMs: number;
  value: number;
  unit: string;
}

/** Include selected results without adding them to default-branch history. */
export function comparisonTimeRange(historyTimes: number[], markers: ComparisonMarker[]): ValueRange | null {
  const extent = finiteValueExtent([...historyTimes, ...markers.map((marker) => marker.chartMs)]);
  if (extent === null) return null;
  // Room for the endpoint symbols; a lone instant still needs a real time axis.
  const padding = Math.max((extent.max - extent.min) * 0.1, 60_000);
  return { min: (extent.min - padding) / 1000, max: (extent.max + padding) / 1000 };
}

function finiteValueExtent(values: readonly number[]): ValueRange | null {
  let min = Number.POSITIVE_INFINITY;
  let max = Number.NEGATIVE_INFINITY;
  for (const value of values) {
    if (!Number.isFinite(value)) continue;
    min = Math.min(min, value);
    max = Math.max(max, value);
  }
  return Number.isFinite(min) && Number.isFinite(max) ? { min, max } : null;
}

export function clampRangeToDomain(
  range: ValueRange | null,
  domain: ValueRange | null,
): ValueRange | null {
  if (range === null || domain === null) return null;
  const min = Math.max(range.min, domain.min);
  const max = Math.min(range.max, domain.max);
  if (!Number.isFinite(min) || !Number.isFinite(max) || min >= max) return null;
  if (min === range.min && max === range.max) return range;
  return { min, max };
}

function tooltipUsableWidth(containerWidth: number, tooltipWidth: number, margin: number): number {
  return Math.min(tooltipWidth, Math.max(0, containerWidth - margin * 2));
}

export function tooltipLeftForCursor(
  left: number,
  containerWidth: number,
  tooltipWidth = 448,
  margin = 8,
): number {
  if (!Number.isFinite(left) || !Number.isFinite(containerWidth) || containerWidth <= 0) {
    return margin;
  }
  const usableWidth = tooltipUsableWidth(containerWidth, tooltipWidth, margin);
  const minLeft = margin;
  const maxLeft = Math.max(minLeft, containerWidth - usableWidth - margin);
  return Math.min(maxLeft, Math.max(minLeft, left + margin));
}

export type TooltipVerticalPlacement = "above" | "below" | "clamped";

export interface TooltipVerticalPosition {
  top: number;
  placement: TooltipVerticalPlacement;
}

export function tooltipTopForCursor(
  top: number,
  containerHeight: number,
  tooltipHeight = 128,
  margin = 8,
): TooltipVerticalPosition {
  if (!Number.isFinite(top) || !Number.isFinite(containerHeight) || containerHeight <= 0) {
    return { top: margin, placement: "below" };
  }
  const safeTooltipHeight = Number.isFinite(tooltipHeight) && tooltipHeight > 0 ? tooltipHeight : 0;
  const minTop = margin;
  const maxTop = Math.max(minTop, containerHeight - safeTooltipHeight - margin);
  const aboveTop = top - safeTooltipHeight - margin;
  if (aboveTop >= minTop) {
    return { top: aboveTop, placement: "above" };
  }
  const belowTop = top + margin;
  if (belowTop <= maxTop) {
    return { top: belowTop, placement: "below" };
  }
  return {
    top: Math.min(maxTop, Math.max(minTop, top - safeTooltipHeight / 2)),
    placement: "clamped",
  };
}

export function zeroBasedValueRange(
  values: readonly number[],
  padFraction = 0.05,
): ValueRange | null {
  const extent = finiteValueExtent(values);
  if (extent === null) return null;
  const max = Math.max(0, extent.max);
  return { min: 0, max: max === 0 ? 1 : max * (1 + padFraction) };
}

export function observedValueRange(
  values: readonly number[],
  padFraction = 0.05,
): ValueRange | null {
  const extent = finiteValueExtent(values);
  if (extent === null) return null;
  const { min, max } = extent;
  if (min === max) {
    if (min === 0) return { min: 0, max: 1 };
    const pad = Math.abs(min) * padFraction;
    return { min: min - pad, max: max + pad };
  }
  const pad = (max - min) * padFraction;
  return {
    min: min >= 0 ? Math.max(0, min - pad) : min - pad,
    max: max + pad,
  };
}

const MAX_TICK_INTERVALS = 12;

/** niceTicks returns round values (1, 2, or 5 times a power of ten apart)
 * inside range, about target intervals across it, for labeled gridlines. A
 * nonempty range always gets at least two distinct ticks, so a scale shows;
 * when no round step fits, as between adjacent floating-point values, the
 * range's own endpoints are the ticks. */
export function niceTicks(range: ValueRange, target = 3): number[] {
  const span = range.max - range.min;
  if (!Number.isFinite(span) || span <= 0) return [range.min];
  for (let intervals = target; intervals <= MAX_TICK_INTERVALS; intervals *= 2) {
    const ticks = ticksAtStep(range, niceStep(span / intervals));
    if (ticks.length >= 2) return ticks;
  }
  return [range.min, range.max];
}

function niceStep(raw: number): number {
  const magnitude = 10 ** Math.floor(Math.log10(raw));
  return [1, 2, 5, 10].map((multiple) => multiple * magnitude).find((candidate) => candidate >= raw)!;
}

// Every tick is a whole multiple of the step's power of ten, so rounding to
// that many decimals removes floating-point noise without merging neighbours.
// The tick count is bounded, and a tick that does not advance past the last
// one, as when k * step loses precision at a large k, ends the list.
function ticksAtStep(range: ValueRange, step: number): number[] {
  const decimals = Math.min(100, Math.max(0, -Math.floor(Math.log10(step))));
  const first = Math.ceil(range.min / step);
  const ticks: number[] = [];
  for (let i = 0; i <= MAX_TICK_INTERVALS * 2; i++) {
    const tick = Number(((first + i) * step).toFixed(decimals));
    if (tick > range.max) break;
    if (ticks.length > 0 && tick <= ticks[ticks.length - 1]!) break;
    if (tick >= range.min) ticks.push(tick);
  }
  return ticks;
}
