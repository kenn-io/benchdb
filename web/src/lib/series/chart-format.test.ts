import { describe, expect, it } from "vitest";

import { axisTickLabels, compactAxisValue } from "./chart-format";

describe("compactAxisValue", () => {
  it("formats large throughput values without long clipped labels", () => {
    expect(compactAxisValue(607_822_601.56)).toBe("608M");
    expect(compactAxisValue(1_397_000_000)).toBe("1.4B");
  });

  it("keeps small values readable", () => {
    expect(compactAxisValue(0)).toBe("0");
    expect(compactAxisValue(0.01234)).toBe("0.0123");
    expect(compactAxisValue(-42.4)).toBe("-42.4");
  });

  it("keeps distinct sub-thousandth ticks distinct", () => {
    expect(compactAxisValue(0.00012)).toBe("0.00012");
    expect(compactAxisValue(0.00014)).toBe("0.00014");
  });
});

describe("axisTickLabels", () => {
  it("labels sub-millisecond second ticks in microseconds", () => {
    expect(axisTickLabels([0, 0.0001, 0.0002, 0.0003], "s")).toEqual(["0 µs", "100 µs", "200 µs", "300 µs"]);
  });

  it("labels millisecond and nanosecond ranges with one shared scale", () => {
    expect(axisTickLabels([0.002, 0.0025, 0.003], "s")).toEqual(["2 ms", "2.5 ms", "3 ms"]);
    expect(axisTickLabels([0, 2e-8, 4e-8], "s")).toEqual(["0 ns", "20 ns", "40 ns"]);
  });

  it("keeps whole-second ticks in seconds", () => {
    expect(axisTickLabels([0, 0.5, 1, 1.5], "s")).toEqual(["0 s", "0.5 s", "1 s", "1.5 s"]);
  });

  it("uses byte units for byte ticks and compact numbers otherwise", () => {
    expect(axisTickLabels([1_500_000], "B")).toEqual(["1.5 MB"]);
    expect(axisTickLabels([1_200_000_000], "B/s")).toEqual(["1.2B"]);
  });
});
