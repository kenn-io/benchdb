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
});
