interface AxisScale {
  min: number;
  factor: number;
  suffix: string;
}

const SECOND_SCALES: readonly AxisScale[] = [
  { min: 1, factor: 1, suffix: " s" },
  { min: 1e-3, factor: 1e3, suffix: " ms" },
  { min: 1e-6, factor: 1e6, suffix: " µs" },
  { min: 0, factor: 1e9, suffix: " ns" },
];

const BYTE_SCALES: readonly AxisScale[] = [
  { min: 1e12, factor: 1e-12, suffix: " TB" },
  { min: 1e9, factor: 1e-9, suffix: " GB" },
  { min: 1e6, factor: 1e-6, suffix: " MB" },
  { min: 1e3, factor: 1e-3, suffix: " kB" },
  { min: 0, factor: 1, suffix: " B" },
];

const COMPACT_SCALES: readonly AxisScale[] = [
  { min: 1e9, factor: 1e-9, suffix: "B" },
  { min: 1e6, factor: 1e-6, suffix: "M" },
  { min: 1e3, factor: 1e-3, suffix: "k" },
  { min: 0, factor: 1, suffix: "" },
];

// A double carries 15 to 16 significant digits; labels never ask for more.
const MAX_SIGNIFICANT_DIGITS = 15;
const MAX_FRACTION_DIGITS = 20;

/** axisTickLabels labels one axis's ticks with one shared scale chosen from
 * the largest tick: seconds become s, ms, µs, or ns, bytes become B through TB,
 * and other values use k, M, or B. Every label gets the fewest decimals that
 * keep all ticks exact, so a narrow observed range stays readable. */
export function axisTickLabels(ticks: number[], unit: string | null): string[] {
  const scales = unit === "s" ? SECOND_SCALES : unit === "B" ? BYTE_SCALES : COMPACT_SCALES;
  const largest = Math.max(0, ...ticks.map((tick) => Math.abs(tick)));
  const scale = scales.find((candidate) => largest >= candidate.min) ?? scales[scales.length - 1]!;
  const scaled = ticks.map((tick) => tick * scale.factor);
  const digits = fractionDigits(scaled);
  return scaled.map((value) => `${trimZeros(value.toFixed(digits))}${scale.suffix}`);
}

// fractionDigits picks the fewest decimals at which every tick is exact,
// judged against the spacing between ticks rather than each tick's size: a
// 100-byte step at 600 GB is tiny relative to the value but is the whole
// difference between neighbouring labels. Distinct ticks never share a label.
function fractionDigits(values: number[]): number {
  const gap = smallestGap(values);
  const largest = Math.max(...values.map((value) => Math.abs(value)));
  const integerDigits = largest >= 1 ? Math.floor(Math.log10(largest)) + 1 : 0;
  const leadingZeros = largest > 0 && largest < 1 ? -Math.floor(Math.log10(largest)) - 1 : 0;
  const limit = Math.min(MAX_FRACTION_DIGITS, Math.max(0, MAX_SIGNIFICANT_DIGITS - integerDigits + leadingZeros));
  let digits = 0;
  while (digits < limit && !values.every((value) => exactAt(value, digits, gap))) digits++;
  while (digits < limit && !labelsDistinct(values, digits)) digits++;
  return digits;
}

function smallestGap(values: number[]): number {
  const sorted = [...new Set(values)].sort((a, b) => a - b);
  let gap = Infinity;
  for (let i = 1; i < sorted.length; i++) gap = Math.min(gap, sorted[i]! - sorted[i - 1]!);
  return gap;
}

function exactAt(value: number, digits: number, gap: number): boolean {
  const scaled = value * 10 ** digits;
  const error = Math.abs(scaled - Math.round(scaled));
  const rounding = 4 * Number.EPSILON * Math.abs(scaled);
  const spacing = Number.isFinite(gap) ? 1e-6 * gap * 10 ** digits : 0;
  return error <= Math.max(rounding, spacing);
}

function labelsDistinct(values: number[], digits: number): boolean {
  return new Set(values.map((value) => value.toFixed(digits))).size === new Set(values).size;
}

function trimZeros(text: string): string {
  return text.includes(".") ? text.replace(/\.?0+$/, "") : text;
}

const MIN_AXIS_SIZE = 56;
const AXIS_CHAR_WIDTH = 7;
const AXIS_PADDING = 22;

/** axisSize returns a y-axis width in CSS pixels that fits its longest label,
 * so long precise labels are not clipped. values is null before uPlot formats
 * the first ticks. */
export function axisSize(values: string[] | null): number {
  const longest = Math.max(0, ...(values ?? []).map((value) => value.length));
  return Math.max(MIN_AXIS_SIZE, longest * AXIS_CHAR_WIDTH + AXIS_PADDING);
}
