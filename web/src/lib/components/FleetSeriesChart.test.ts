import { fireEvent, render, screen } from "@testing-library/svelte";
import { tick } from "svelte";
import { describe, expect, it, vi } from "vitest";

import type { MachineTrack } from "../series/loader";
import type { SeriesPoint } from "../series/transform";

const plotState = vi.hoisted(() => ({
  options: undefined as Record<string, unknown> | undefined,
  data: undefined as unknown,
  cursorHook: undefined as ((plot: unknown) => void) | undefined,
  instance: undefined as unknown,
}));

vi.mock("uplot", () => {
  class MockUPlot {
    bbox = { left: 0, top: 0, width: 640, height: 320 };
    cursor = { idx: 0, left: 100, top: 120 };

    constructor(options: Record<string, unknown>, data: unknown) {
      plotState.options = options;
      plotState.data = data;
      const hooks = options["hooks"] as { setCursor?: ((plot: unknown) => void)[] } | undefined;
      plotState.cursorHook = hooks?.setCursor?.[0];
      plotState.instance = this;
    }

    destroy() {}
    setSize() {}
    posToVal() { return 1.1; }
  }
  return { default: MockUPlot };
});

import FleetSeriesChart from "./FleetSeriesChart.svelte";

function point(resultId: string, machineValue: number): SeriesPoint {
  return {
    resultId,
    commitHash: "abc1234",
    commitMessage: "tune benchmark",
    commitTimestampMs: Date.parse("2026-01-01T00:00:00Z"),
    resultTimestampMs: Date.parse("2026-01-01T00:01:00Z"),
    chartMs: Date.parse("2026-01-01T00:00:00Z"),
    measurements: [machineValue],
    svs: machineValue,
    unit: "s",
    runTags: {},
    info: {},
    changeAnnotations: {},
    stats: {
      z: 1,
      rollingMean: machineValue - 0.1,
      rollingStddev: 0.05,
      isOutlier: false,
      isStep: false,
      beginsChange: false,
      segmentId: 1,
    },
  };
}

const tracks: MachineTrack[] = [
  { machineName: "machine-a", segments: [], points: [point("result-a", 1.1)] },
  { machineName: "machine-b", segments: [], points: [point("result-b", 2.2)] },
];

describe("FleetSeriesChart", () => {
  it("renders a zero-based scale with a rolling mean and range for each machine", () => {
    render(FleetSeriesChart, { props: { tracks, sigma: 2 } });
    const options = plotState.options as {
      scales: { y: { range: () => number[] } };
      series: { label?: string }[];
      bands: unknown[];
    };
    expect(options.scales.y.range()[0]).toBe(0);
    expect(options.series.map((series) => series.label)).toContain("machine-a rolling mean");
    expect(options.series.map((series) => series.label)).toContain("machine-b rolling mean");
    expect(options.bands).toHaveLength(2);
    expect(screen.getByText("rolling mean")).toBeInTheDocument();
    expect(screen.getByText("2σ range")).toBeInTheDocument();
  });

  it("shows the nearest machine point and opens its result on click", async () => {
    const onopen = vi.fn();
    const rect = vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue({
      x: 0, y: 0, top: 0, right: 640, bottom: 320, left: 0, width: 640, height: 320,
      toJSON: () => ({}),
    });
    const { container } = render(FleetSeriesChart, { props: { tracks, onopen } });
    const chart = container.querySelector(".fleet-chart")!;
    Object.defineProperty(chart, "clientHeight", { value: 400 });
    Object.defineProperty(chart, "clientWidth", { value: 640 });
    plotState.cursorHook?.(plotState.instance);
    await tick();
    await tick();
    expect(screen.getByText("machine-a", { selector: ".tip strong" })).toBeInTheDocument();
    expect(screen.getByText(/abc1234 · 1\.1 s/)).toBeInTheDocument();
    await fireEvent.click(chart);
    expect(onopen).toHaveBeenCalledWith("result-a");
    rect.mockRestore();
  });
});
