import { describe, expect, it } from "vitest";

import { axisSize, axisTickLabels } from "./chart-format";

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

  it("keeps narrow observed ranges distinct", () => {
    expect(axisTickLabels([0.00000101, 0.00000102, 0.00000103, 0.00000104], "s"))
      .toEqual(["1.01 µs", "1.02 µs", "1.03 µs", "1.04 µs"]);
    expect(axisTickLabels([1_011_500_000, 1_012_000_000, 1_012_500_000], "B/s"))
      .toEqual(["1.0115B", "1.012B", "1.0125B"]);
    expect(axisTickLabels([0, 0.25, 0.5, 0.75], null)).toEqual(["0", "0.25", "0.5", "0.75"]);
  });

  it("keeps ticks distinct when they differ beyond six decimals of the shared scale", () => {
    expect(axisTickLabels([1_000_000_100, 1_000_000_200, 1_000_000_300], "B"))
      .toEqual(["1.0000001 GB", "1.0000002 GB", "1.0000003 GB"]);
  });

  it("uses byte units for byte ticks and compact numbers otherwise", () => {
    expect(axisTickLabels([1_000_000, 1_500_000, 2_000_000], "B")).toEqual(["1 MB", "1.5 MB", "2 MB"]);
    expect(axisTickLabels([600_000_000, 605_000_000], "B/s")).toEqual(["600M", "605M"]);
    expect(axisTickLabels([0, 400, 800], null)).toEqual(["0", "400", "800"]);
    expect(axisTickLabels([-40, -20, 0], null)).toEqual(["-40", "-20", "0"]);
  });
});

describe("axisSize", () => {
  it("widens the axis for longer labels and keeps a minimum", () => {
    expect(axisSize(null)).toBe(56);
    expect(axisSize(["0", "1"])).toBe(56);
    const short = axisSize(["1 GB", "2 GB"]);
    const long = axisSize(["1.0000001 GB", "1.0000002 GB"]);
    expect(long).toBeGreaterThan(short);
  });
});
