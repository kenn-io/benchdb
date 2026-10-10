const INTEGER_FORMAT = new Intl.NumberFormat("en-US", {
  maximumFractionDigits: 0,
  useGrouping: true,
});

const DECIMAL_FORMAT = new Intl.NumberFormat("en-US", {
  maximumFractionDigits: 20,
  useGrouping: true,
});

const BYTE_FORMAT = new Intl.NumberFormat("en-US", {
  maximumSignificantDigits: 5,
  useGrouping: true,
});

const BYTE_UNITS = [
  { threshold: 1_000_000_000_000, divisor: 1_000_000_000_000, unit: "TB" },
  { threshold: 1_000_000_000, divisor: 1_000_000_000, unit: "GB" },
  { threshold: 1_000_000, divisor: 1_000_000, unit: "MB" },
  { threshold: 1_000, divisor: 1_000, unit: "kB" },
] as const;

/** formatNumber keeps integer benchmark values exact while rendering decimal
 * values compactly enough for dense benchmark tables. */
export function formatNumber(value: number, significantDigits = 4): string {
  if (!Number.isFinite(value)) {
    return String(value);
  }
  if (Number.isInteger(value)) {
    return INTEGER_FORMAT.format(value);
  }
  const rounded = Number(value.toPrecision(significantDigits));
  return Number.isInteger(rounded) ? INTEGER_FORMAT.format(rounded) : DECIMAL_FORMAT.format(rounded);
}

export function formatBytes(value: number): string {
  const abs = Math.abs(value);
  const scale = BYTE_UNITS.find((candidate) => abs >= candidate.threshold);
  if (scale === undefined) {
    return `${formatNumber(value)} B`;
  }
  return `${BYTE_FORMAT.format(value / scale.divisor)} ${scale.unit}`;
}

const DURATION_UNITS = [
  { threshold: 1, perSecond: 1, unit: "s" },
  { threshold: 1e-3, perSecond: 1e3, unit: "ms" },
  { threshold: 1e-6, perSecond: 1e6, unit: "µs" },
] as const;

/** formatDuration renders a time measurement in the largest of s, ms, µs, or
 * ns that keeps its magnitude at or above one. Zero and non-finite values keep
 * the submitted unit. */
function formatDuration(value: number, unit: "s" | "ns"): string {
  if (value === 0 || !Number.isFinite(value)) {
    return `${formatNumber(value)} ${unit}`;
  }
  const seconds = unit === "s" ? value : value / 1e9;
  const magnitude = Math.abs(Number(seconds.toPrecision(4)));
  const scale = DURATION_UNITS.find((candidate) => magnitude >= candidate.threshold);
  if (scale === undefined) {
    return `${formatNumber(unit === "ns" ? value : seconds * 1e9)} ns`;
  }
  return `${formatNumber(scale.unit === unit ? value : seconds * scale.perSecond)} ${scale.unit}`;
}

export function formatMeasurement(value: number | null, unit: string | null, missing = "—"): string {
  if (value === null) {
    return missing;
  }
  if (unit === "B") {
    return formatBytes(value);
  }
  if (unit === "s" || unit === "ns") {
    return formatDuration(value, unit);
  }
  const text = formatNumber(value);
  return unit === null ? text : `${text} ${unit}`;
}

export function exactMeasurement(value: number, unit: string | null): string {
  const text = formatNumber(value, 20);
  return unit === null ? text : `${text} ${unit}`;
}

export function clipboardMeasurementValue(value: number): string {
  return String(value);
}

const RELATIVE_UNITS: Array<{ unit: Intl.RelativeTimeFormatUnit; seconds: number }> = [
  { unit: "year", seconds: 365 * 24 * 3600 },
  { unit: "month", seconds: 30 * 24 * 3600 },
  { unit: "week", seconds: 7 * 24 * 3600 },
  { unit: "day", seconds: 24 * 3600 },
  { unit: "hour", seconds: 3600 },
  { unit: "minute", seconds: 60 },
];

/** relativeTime renders how long ago an instant was ("3 hours ago",
 * "yesterday") in the largest whole unit; anything under a minute is "just
 * now". A future instant can only come from clock skew between the submitter
 * and this browser, so it also reads "just now". now and locale are
 * injectable for tests. */
export function relativeTime(iso: string, now: Date = new Date(), locale?: string): string {
  const seconds = Math.min(0, (Date.parse(iso) - now.getTime()) / 1000);
  const format = new Intl.RelativeTimeFormat(locale, { numeric: "auto" });
  for (const { unit, seconds: size } of RELATIVE_UNITS) {
    if (Math.abs(seconds) >= size) return format.format(Math.round(seconds / size), unit);
  }
  return "just now";
}
