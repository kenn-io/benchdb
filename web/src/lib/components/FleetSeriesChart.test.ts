import { fireEvent, render, screen } from "@testing-library/svelte";
import { tick } from "svelte";
import { describe, expect, it, vi } from "vitest";

import type { MachineTrack } from "../series/loader";
import type { SeriesPoint } from "../series/transform";

const plotState = vi.hoisted(() => ({
  options: undefined as Record<string, unknown> | undefined,
  data: undefined as unknown,
  cursorHook: undefined as ((plot: unknown) => void) | undefined,
  selectHook: undefined as ((plot: unknown) => void) | undefined,
  scaleCalls: [] as { key: string; limits: { min: number; max: number } }[],
  instance: undefined as unknown,
  xForPosition: (position: number) => position,
}));

vi.mock("uplot", () => {
  class MockUPlot {
    bbox = { left: 0, top: 0, width: 640, height: 320 };
    cursor = { idx: 0, left: 100, top: 120 };
    select = { left: 100, top: 0, width: 200, height: 320 };

    constructor(options: Record<string, unknown>, data: unknown) {
      plotState.options = options;
      plotState.data = data;
      const hooks = options["hooks"] as {
        setCursor?: ((plot: unknown) => void)[];
        setSelect?: ((plot: unknown) => void)[];
      } | undefined;
      plotState.cursorHook = hooks?.setCursor?.[0];
      plotState.selectHook = hooks?.setSelect?.[0];
      plotState.instance = this;
    }

    destroy() {}
    setSize() {}
    posToVal(position: number, scale: string) {
      return scale === "x" ? plotState.xForPosition(position) : 1.1;
    }
    setScale(key: string, limits: { min: number; max: number }) {
      plotState.scaleCalls.push({ key, limits });
    }
  }
  return { default: MockUPlot };
});

import FleetSeriesChart from "./FleetSeriesChart.svelte";

function point(
  resultId: string,
  machineValue: number,
  timestamp = "2026-01-01T00:00:00Z",
): SeriesPoint {
  return {
    resultId,
    commitHash: `sha-${resultId}`,
    commitMessage: "tune benchmark",
    commitTimestampMs: Date.parse(timestamp),
    resultTimestampMs: Date.parse("2026-01-01T00:01:00Z"),
    chartMs: Date.parse(timestamp),
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
      scales: { x: { time: boolean }; y: { range: () => number[] } };
      series: { label?: string }[];
      bands: unknown[];
    };
    expect(options.scales.y.range()[0]).toBe(0);
    expect(options.scales.x.time).toBe(true);
    expect(options.series.map((series) => series.label)).toContain("machine-a rolling mean");
    expect(options.series.map((series) => series.label)).toContain("machine-b rolling mean");
    expect(options.bands).toHaveLength(2);
    expect(screen.getByText("rolling mean")).toBeInTheDocument();
    expect(screen.getByText("2σ range")).toBeInTheDocument();
  });

  it("can scale the fleet Y-axis from the observed minimum", () => {
    render(FleetSeriesChart, { props: { tracks, sigma: 2, zeroBased: false } });
    const options = plotState.options as { scales: { y: { range: () => number[] } } };
    expect(options.scales.y.range()[0]).toBeGreaterThan(0);
  });

  it("spaces fleet points by elapsed calendar time", () => {
    render(FleetSeriesChart, {
      props: {
        tracks: [{
          machineName: "machine-a",
          segments: [],
          points: [
            point("day-1", 1, "2026-01-01T00:00:00Z"),
            point("day-2", 2, "2026-01-02T00:00:00Z"),
            point("day-11", 3, "2026-01-11T00:00:00Z"),
          ],
        }],
      },
    });
    const [xs] = plotState.data as [number[]];
    expect(xs[1]! - xs[0]!).toBe(24 * 60 * 60);
    expect(xs[2]! - xs[1]!).toBe(9 * 24 * 60 * 60);
  });

  it("preserves reruns of the same commit at the same timestamp", () => {
    const first = point("rerun-a", 1);
    const second = point("rerun-b", 2);
    second.commitHash = first.commitHash;
    render(FleetSeriesChart, {
      props: {
        tracks: [{ machineName: "machine-a", segments: [], points: [first, second] }],
      },
    });

    const [xs, values] = plotState.data as [number[], (number | null)[]];
    expect(xs).toEqual([first.chartMs / 1000, second.chartMs / 1000]);
    expect(values).toEqual([1, 2]);
  });

  it("redraws when a live refresh adds a result", async () => {
    const { rerender } = render(FleetSeriesChart, {
      props: {
        tracks: [{
          machineName: "machine-a",
          segments: [],
          points: [point("day-1", 1, "2026-01-01T00:00:00Z")],
        }],
      },
    });
    expect((plotState.data as [number[]])[0]).toHaveLength(1);

    await rerender({
      tracks: [{
        machineName: "machine-a",
        segments: [],
        points: [
          point("day-1", 1, "2026-01-01T00:00:00Z"),
          point("day-2", 2, "2026-01-02T00:00:00Z"),
        ],
      }],
    });
    await tick();

    expect((plotState.data as [number[]])[0]).toHaveLength(2);
  });

  it("zooms by horizontal brush and resets to the full time range", async () => {
    plotState.scaleCalls = [];
    render(FleetSeriesChart, {
      props: {
        tracks: [{
          machineName: "machine-a",
          segments: [],
          points: [
            point("day-1", 1, "2026-01-01T00:00:00Z"),
            point("day-11", 3, "2026-01-11T00:00:00Z"),
          ],
        }],
      },
    });
    const options = plotState.options as { cursor: { drag: { x: boolean; y: boolean; dist: number } } };
    expect(options.cursor.drag).toMatchObject({ x: true, y: false, dist: 8 });

    plotState.selectHook?.(plotState.instance);
    await tick();
    await fireEvent.click(screen.getByRole("button", { name: "Reset zoom" }));

    expect(plotState.scaleCalls).toEqual([{
      key: "x",
      limits: {
        min: Date.parse("2026-01-01T00:00:00Z") / 1000,
        max: Date.parse("2026-01-11T00:00:00Z") / 1000,
      },
    }]);
    expect(screen.getByText("Drag horizontally to zoom")).toBeInTheDocument();
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
    expect(screen.getByText(/sha-result-a · 1\.1 s/)).toBeInTheDocument();
    await fireEvent.click(chart);
    expect(onopen).toHaveBeenCalledWith("result-a");
    rect.mockRestore();
  });

  it("clears a zoom window outside replacement fleet data", async () => {
    const first = point("day-1", 1, "2026-01-01T00:00:00Z");
    const last = point("day-11", 3, "2026-01-11T00:00:00Z");
    const { rerender } = render(FleetSeriesChart, {
      props: { tracks: [{ machineName: "machine-a", segments: [], points: [first, last] }] },
    });
    plotState.xForPosition = (position) =>
      position === 100 ? first.chartMs / 1000 : last.chartMs / 1000;
    plotState.selectHook?.(plotState.instance);
    await tick();
    expect(screen.getByRole("button", { name: "Reset zoom" })).toBeInTheDocument();

    await rerender({
      tracks: [{
        machineName: "machine-b",
        segments: [],
        points: [
          point("day-366", 2, "2027-01-01T00:00:00Z"),
          point("day-376", 4, "2027-01-11T00:00:00Z"),
        ],
      }],
    });
    await tick();

    expect(screen.getByText("Drag horizontally to zoom")).toBeInTheDocument();
    plotState.xForPosition = (position) => position;
  });
});
