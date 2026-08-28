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

export function formatMeasurement(value: number | null, unit: string | null, missing = "—"): string {
  if (value === null) {
    return missing;
  }
  if (unit === "B") {
    return formatBytes(value);
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
