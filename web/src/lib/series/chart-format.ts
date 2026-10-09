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

const MAX_FRACTION_DIGITS = 6;

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

function fractionDigits(values: number[]): number {
  for (let digits = 0; digits < MAX_FRACTION_DIGITS; digits++) {
    const power = 10 ** digits;
    if (values.every((value) => isWhole(value * power))) return digits;
  }
  return MAX_FRACTION_DIGITS;
}

function isWhole(value: number): boolean {
  return Math.abs(value - Math.round(value)) < 1e-9 * Math.max(1, Math.abs(value));
}

function trimZeros(text: string): string {
  return text.includes(".") ? text.replace(/\.?0+$/, "") : text;
}
