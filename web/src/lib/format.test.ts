import { describe, expect, it } from "vitest";

import {
  clipboardMeasurementValue,
  exactMeasurement,
  formatMeasurement,
} from "./format";

describe("measurement formatting", () => {
  it("renders bytes with five significant digits and preserves the exact value separately", () => {
    expect(formatMeasurement(12_263_215_104, "B")).toBe("12.263 GB");
    expect(exactMeasurement(12_263_215_104, "B")).toBe("12,263,215,104 B");
    expect(clipboardMeasurementValue(12_263_215_104)).toBe("12263215104");
  });

  it("keeps small byte counts in bytes", () => {
    expect(formatMeasurement(999, "B")).toBe("999 B");
  });

  it.each([
    [0.00007772, "77.72 µs"],
    [0.0001304, "130.4 µs"],
    [0.005213, "5.213 ms"],
    [150.8, "150.8 s"],
    [3600, "3,600 s"],
    [2.5e-10, "0.25 ns"],
  ])("scales seconds %s to %s", (value, text) => {
    expect(formatMeasurement(value, "s")).toBe(text);
  });

  it.each([
    [1, "1 s"],
    [0.001, "1 ms"],
    [0.000001, "1 µs"],
    [0.999, "999 ms"],
    [0.000999, "999 µs"],
    [0.000000999, "999 ns"],
  ])("switches second units at exact boundaries: %s s is %s", (value, text) => {
    expect(formatMeasurement(value, "s")).toBe(text);
  });

  it("moves to the larger unit when rounding reaches the boundary", () => {
    expect(formatMeasurement(0.99996, "s")).toBe("1 s");
    expect(formatMeasurement(0.00099996, "s")).toBe("1 ms");
  });

  it("keeps the sign of negative durations while scaling by magnitude", () => {
    expect(formatMeasurement(-0.003878, "s")).toBe("-3.878 ms");
    expect(formatMeasurement(-0.000002119, "s")).toBe("-2.119 µs");
    expect(formatMeasurement(-21.05, "s")).toBe("-21.05 s");
  });

  it("renders zero in the submitted time unit", () => {
    expect(formatMeasurement(0, "s")).toBe("0 s");
    expect(formatMeasurement(0, "ns")).toBe("0 ns");
  });

  it("scales nanoseconds with the same rule as seconds", () => {
    expect(formatMeasurement(834, "ns")).toBe("834 ns");
    expect(formatMeasurement(1_000, "ns")).toBe("1 µs");
    expect(formatMeasurement(52_651_400, "ns")).toBe("52.65 ms");
    expect(formatMeasurement(2_500_000_000, "ns")).toBe("2.5 s");
    expect(formatMeasurement(-77_720, "ns")).toBe("-77.72 µs");
  });

  it("keeps exact and clipboard values in the submitted unit", () => {
    expect(exactMeasurement(0.00007772, "s")).toBe("0.00007772 s");
    expect(clipboardMeasurementValue(0.00007772)).toBe("0.00007772");
    expect(exactMeasurement(-0.003878, "s")).toBe("-0.003878 s");
    expect(exactMeasurement(52_651_400, "ns")).toBe("52,651,400 ns");
    expect(clipboardMeasurementValue(52_651_400)).toBe("52651400");
  });

  it("leaves other units unscaled", () => {
    expect(formatMeasurement(0.00007772, "i/s")).toBe("0.00007772 i/s");
    expect(formatMeasurement(1_849_000_000, "B/s")).toBe("1,849,000,000 B/s");
    expect(formatMeasurement(0.005213, null)).toBe("0.005213");
  });
});
