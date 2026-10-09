import { formatMeasurement } from "../format";

const SECOND_SCALES = [
  { min: 1, factor: 1, suffix: "s" },
  { min: 1e-3, factor: 1e3, suffix: "ms" },
  { min: 1e-6, factor: 1e6, suffix: "µs" },
] as const;
const NANOSECONDS = { factor: 1e9, suffix: "ns" } as const;

export function compactAxisValue(value: number): string {
  const abs = Math.abs(value);
  if (abs >= 1_000_000_000) {
    return `${trimFixed(value / 1_000_000_000, 1)}B`;
  }
  if (abs >= 1_000_000) {
    return `${trimFixed(value / 1_000_000, 0)}M`;
  }
  if (abs >= 1_000) {
    return `${trimFixed(value / 1_000, 0)}k`;
  }
  if (abs > 0 && abs < 0.1) {
    return String(Number(value.toPrecision(3)));
  }
  if (Number.isInteger(value)) {
    return String(value);
  }
  return trimFixed(value, 1);
}

/** axisTickLabels labels one axis's ticks. Byte ticks use byte units, and
 * second ticks share the largest tick's scale (s, ms, µs, or ns) so
 * sub-millisecond histories stay readable. */
export function axisTickLabels(ticks: number[], unit: string | null): string[] {
  if (unit === "B") {
    return ticks.map((tick) => formatMeasurement(tick, "B"));
  }
  if (unit === "s") {
    const largest = Math.max(0, ...ticks.map((tick) => Math.abs(tick)));
    const scale = SECOND_SCALES.find((candidate) => largest >= candidate.min) ?? NANOSECONDS;
    return ticks.map((tick) => `${compactAxisValue(tick * scale.factor)} ${scale.suffix}`);
  }
  return ticks.map((tick) => compactAxisValue(tick));
}

function trimFixed(value: number, fractionDigits: number): string {
  return value
    .toFixed(fractionDigits)
    .replace(/\.0+$/, "")
    .replace(/(\.\d*?)0+$/, "$1");
}
