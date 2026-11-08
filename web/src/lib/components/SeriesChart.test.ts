import { fireEvent, render, screen } from "@testing-library/svelte";
import { tick } from "svelte";
import { describe, expect, it, vi } from "vitest";

import type { SeriesPoint } from "../series/transform";

const plotState = vi.hoisted(() => ({
  cursorHook: undefined as ((plot: unknown) => void) | undefined,
  selectHook: undefined as ((plot: unknown) => void) | undefined,
  scaleCalls: [] as { key: string; limits: { min: number; max: number } }[],
  instance: undefined as unknown,
  options: undefined as Record<string, unknown> | undefined,
  xForPosition: (position: number) => position,
}));

vi.mock("uplot", () => {
  class MockUPlot {
    data: unknown;
    bbox = { left: 0, top: 0, width: 640, height: 280 };
    cursor = { left: 0, top: 0 };
    select = { left: 100, top: 0, width: 200, height: 280 };

    constructor(options: { hooks?: {
      setCursor?: ((plot: unknown) => void)[];
      setSelect?: ((plot: unknown) => void)[];
    } }, data: unknown) {
      this.data = data;
      plotState.options = options;
      plotState.cursorHook = options.hooks?.setCursor?.[0];
      plotState.selectHook = options.hooks?.setSelect?.[0];
      plotState.instance = this;
    }

    destroy() {}
    redraw() {}
    setSelect() {}
    setSize() {}
    posToVal(position: number, scale: string) {
      return scale === "x" ? plotState.xForPosition(position) : 1;
    }
    setScale(key: string, limits: { min: number; max: number }) {
      plotState.scaleCalls.push({ key, limits });
    }
  }
  return { default: MockUPlot };
});

import SeriesChart from "./SeriesChart.svelte";

const boundaryPoint: SeriesPoint = {
  resultId: "r1",
  commitHash: "abc1234",
  commitMessage: "update benchmark setup",
  commitTimestampMs: Date.parse("2026-01-01T00:00:00Z"),
  resultTimestampMs: Date.parse("2026-01-01T00:01:00Z"),
  chartMs: Date.parse("2026-01-01T00:00:00Z"),
  measurements: [1],
  svs: 1,
  unit: "s",
  runTags: { channel: "nightly" },
  info: { build: "release" },
  changeAnnotations: { begins_distribution_change: true },
  stats: {
    z: null,
    rollingMean: 1,
    rollingStddev: null,
    isOutlier: false,
    isStep: false,
    beginsChange: true,
    fixedBaseline: false,
    segmentId: 1,
  },
};

describe("SeriesChart", () => {
  it("renders generic metadata in a boundary point tooltip", async () => {
    const rect = vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockReturnValue({
      x: 0,
      y: 0,
      top: 0,
      right: 200,
      bottom: 40,
      left: 0,
      width: 200,
      height: 40,
      toJSON: () => ({}),
    });
    const { container } = render(SeriesChart, { props: { points: [boundaryPoint] } });
    const chartWrap = container.querySelector(".chart-wrap");
    expect(chartWrap).not.toBeNull();
    Object.defineProperty(chartWrap, "clientHeight", { value: 400 });
    Object.defineProperty(chartWrap, "clientWidth", { value: 640 });
    (plotState.instance as { cursor: { top: number } }).cursor.top = 150;
    expect(plotState.cursorHook).toBeTypeOf("function");
    plotState.cursorHook?.(plotState.instance);
    await tick();
    await tick();
    expect(screen.getByText("info: build=release")).toBeInTheDocument();
    expect(screen.getByText("run: channel=nightly")).toBeInTheDocument();
    expect(container.querySelector(".tip")).toHaveStyle({ top: "102px", visibility: "visible" });
    rect.mockRestore();
  });

  it("starts at zero and opens the hovered result on click", async () => {
    const onopen = vi.fn();
    const { container } = render(SeriesChart, { props: { points: [boundaryPoint], onopen } });
    const options = plotState.options as { scales: { y: { range: () => number[] } } };
    expect(options.scales.y.range()[0]).toBe(0);
    plotState.cursorHook?.(plotState.instance);
    await fireEvent.click(container.querySelector(".chart-wrap")!);
    expect(onopen).toHaveBeenCalledWith("r1");
  });

  it("can scale the Y-axis from the observed minimum", () => {
    render(SeriesChart, { props: { points: [boundaryPoint], zeroBased: false } });
    const options = plotState.options as { scales: { y: { range: () => number[] } } };
    expect(options.scales.y.range()[0]).toBeGreaterThan(0);
  });

  it("zooms by horizontal brush and resets to the full time range", async () => {
    plotState.scaleCalls = [];
    const onopen = vi.fn();
    const laterPoint = {
      ...boundaryPoint,
      resultId: "r2",
      chartMs: Date.parse("2026-01-11T00:00:00Z"),
    };
    const { container } = render(SeriesChart, {
      props: { points: [boundaryPoint, laterPoint], onopen },
    });
    const options = plotState.options as { cursor: { drag: { x: boolean; y: boolean; dist: number } } };
    expect(options.cursor.drag).toMatchObject({ x: true, y: false, dist: 8 });

    plotState.selectHook?.(plotState.instance);
    await tick();
    plotState.cursorHook?.(plotState.instance);
    await tick();
    await fireEvent.click(screen.getByRole("button", { name: "Reset zoom" }));

    expect(plotState.scaleCalls).toEqual([{
      key: "x",
      limits: {
        min: Date.parse("2026-01-01T00:00:00Z") / 1000,
        max: Date.parse("2026-01-11T00:00:00Z") / 1000,
      },
    }]);
    expect(onopen).not.toHaveBeenCalled();
    await fireEvent.click(container.querySelector(".chart-wrap")!);
    expect(onopen).not.toHaveBeenCalled();
  });

  it("resolves hover through the active time scale after zoom", async () => {
    const onopen = vi.fn();
    const middlePoint = {
      ...boundaryPoint,
      resultId: "r2",
      chartMs: Date.parse("2026-01-11T00:00:00Z"),
    };
    const lastPoint = {
      ...boundaryPoint,
      resultId: "r3",
      chartMs: Date.parse("2026-01-21T00:00:00Z"),
    };
    const { container } = render(SeriesChart, {
      props: { points: [boundaryPoint, middlePoint, lastPoint], onopen },
    });
    const plot = plotState.instance as { cursor: { left: number } };
    plotState.xForPosition = (position) =>
      position === 100 ? middlePoint.chartMs / 1000 : lastPoint.chartMs / 1000;
    plotState.selectHook?.(plotState.instance);
    await tick();
    plot.cursor.left = 0;

    plotState.cursorHook?.(plotState.instance);
    await fireEvent.click(container.querySelector(".chart-wrap")!);

    expect(onopen).toHaveBeenCalledWith("r2");
    plotState.xForPosition = (position) => position;
  });

  it("clears a zoom window outside replacement data", async () => {
    const laterPoint = {
      ...boundaryPoint,
      resultId: "r2",
      chartMs: Date.parse("2026-01-11T00:00:00Z"),
    };
    const { rerender } = render(SeriesChart, {
      props: { points: [boundaryPoint, laterPoint] },
    });
    plotState.xForPosition = (position) =>
      position === 100 ? boundaryPoint.chartMs / 1000 : laterPoint.chartMs / 1000;
    plotState.selectHook?.(plotState.instance);
    await tick();
    expect(screen.getByRole("button", { name: "Reset zoom" })).toBeInTheDocument();

    await rerender({
      points: [
        { ...boundaryPoint, resultId: "r3", chartMs: Date.parse("2027-01-01T00:00:00Z") },
        { ...boundaryPoint, resultId: "r4", chartMs: Date.parse("2027-01-11T00:00:00Z") },
      ],
    });
    await tick();

    expect(screen.getByText("Drag horizontally to zoom")).toBeInTheDocument();
    plotState.xForPosition = (position) => position;
  });

  describe("baseline range selection", () => {
    const at = (resultId: string, seconds: number, svs: number): SeriesPoint => ({
      ...boundaryPoint,
      resultId,
      commitHash: `sha-${resultId}`,
      changeAnnotations: {},
      chartMs: seconds * 1000,
      svs,
      measurements: [svs],
      stats: { ...boundaryPoint.stats, beginsChange: false },
    });
    // The mock brush spans 100..300 seconds.
    const points = [at("a", 50, 9), at("b", 150, 2), at("c", 250, 4), at("d", 400, 8)];

    it("selects a dragged range instead of zooming, then applies its average", async () => {
      plotState.scaleCalls = [];
      const onselectbaseline = vi.fn(async () => null);
      const onopen = vi.fn();
      const { container } = render(SeriesChart, { props: { points, onselectbaseline, onopen } });

      await fireEvent.click(screen.getByRole("button", { name: "Select baseline range" }));
      const options = plotState.options as { cursor: { drag: { setScale: boolean } } };
      expect(options.cursor.drag.setScale).toBe(false);
      plotState.selectHook?.(plotState.instance);
      await tick();

      expect(screen.queryByRole("button", { name: "Reset zoom" })).toBeNull();
      expect(screen.getByText("2 results")).toBeTruthy();
      expect(container.querySelector(".baseline-summary")?.textContent).toContain("3");
      expect(screen.getByText(/includes results committed after that start/)).toBeTruthy();
      await fireEvent.click(container.querySelector(".chart-wrap")!);
      expect(onopen).not.toHaveBeenCalled();

      await fireEvent.click(screen.getByRole("button", { name: "Apply baseline" }));
      await tick();
      expect(onselectbaseline).toHaveBeenCalledWith("b", "c");
      expect(screen.queryByRole("group", { name: "Select baseline range" })).toBeNull();
      const rebuilt = plotState.options as { cursor: { drag: { setScale: boolean } } };
      expect(rebuilt.cursor.drag.setScale).toBe(true);
      expect(screen.getByText("Drag horizontally to zoom")).toBeTruthy();
    });

    it("selects a range with the From and Through pickers and shows save errors", async () => {
      const onselectbaseline = vi.fn(async () => "range crosses a boundary");
      render(SeriesChart, { props: { points, onselectbaseline } });
      await fireEvent.click(screen.getByRole("button", { name: "Select baseline range" }));
      const apply = screen.getByRole("button", { name: "Apply baseline" }) as HTMLButtonElement;
      expect(apply.disabled).toBe(true);

      await fireEvent.change(screen.getByLabelText("From"), { target: { value: "d" } });
      await fireEvent.change(screen.getByLabelText("Through"), { target: { value: "a" } });
      expect(screen.getByText("4 results")).toBeTruthy();
      await fireEvent.click(apply);
      await tick();

      expect(onselectbaseline).toHaveBeenCalledWith("a", "d");
      expect(screen.getByRole("alert").textContent).toContain("range crosses a boundary");
      expect(screen.getByRole("group", { name: "Select baseline range" })).toBeTruthy();
    });

    it("blocks a range that crosses a marked distribution change", async () => {
      const marked = [points[0]!, { ...points[1]!, changeAnnotations: { begins_distribution_change: true } }, points[2]!];
      render(SeriesChart, { props: { points: marked, onselectbaseline: vi.fn(async () => null) } });
      await fireEvent.click(screen.getByRole("button", { name: "Select baseline range" }));
      await fireEvent.change(screen.getByLabelText("From"), { target: { value: "a" } });
      await fireEvent.change(screen.getByLabelText("Through"), { target: { value: "c" } });

      expect(screen.getByRole("alert").textContent).toContain("sha-b (1970-01-01 00:02)");
      expect((screen.getByRole("button", { name: "Apply baseline" }) as HTMLButtonElement).disabled).toBe(true);
    });

    it("replaces the baseline on a later result tied on commit time", async () => {
      const tied = [at("a", 100, 2), { ...at("b", 100, 4), changeAnnotations: { baseline_override: { value: 4 } } }];
      const onselectbaseline = vi.fn(async () => null);
      render(SeriesChart, { props: { points: tied, onselectbaseline } });
      await fireEvent.click(screen.getByRole("button", { name: "Select baseline range" }));
      await fireEvent.change(screen.getByLabelText("From"), { target: { value: "b" } });
      await fireEvent.change(screen.getByLabelText("Through"), { target: { value: "b" } });

      expect(screen.queryByRole("alert")).toBeNull();
      await fireEvent.click(screen.getByRole("button", { name: "Apply baseline" }));
      await tick();
      expect(onselectbaseline).toHaveBeenCalledWith("b", "b");
    });

    it("blocks starting beside a tied result that has a baseline", async () => {
      const tied = [at("a", 100, 2), { ...at("b", 100, 4), changeAnnotations: { baseline_override: { value: 4 } } }];
      render(SeriesChart, { props: { points: tied, onselectbaseline: vi.fn(async () => null) } });
      await fireEvent.click(screen.getByRole("button", { name: "Select baseline range" }));
      await fireEvent.change(screen.getByLabelText("From"), { target: { value: "a" } });
      await fireEvent.change(screen.getByLabelText("Through"), { target: { value: "a" } });

      expect(screen.getByRole("alert").textContent).toContain("Start the range from that result");
      expect((screen.getByRole("button", { name: "Apply baseline" }) as HTMLButtonElement).disabled).toBe(true);
    });

    it("shows only the value in a selected baseline's tooltip because the caption explains it", async () => {
      const fixed: SeriesPoint = {
        ...at("b", 150, 2),
        stats: { ...boundaryPoint.stats, fixedBaseline: true, rollingMean: 3, rollingStddev: null, segmentId: 1 },
      };
      render(SeriesChart, { props: { points: [fixed] } });
      plotState.cursorHook?.(plotState.instance);
      await tick();
      await tick();
      expect(screen.getByText("selected baseline 3 s")).toBeInTheDocument();
      expect(screen.queryByText(/includes later results/)).toBeNull();
    });

    it("captions a selected baseline carried by a later tied result", () => {
      const fixed = (p: SeriesPoint): SeriesPoint => ({
        ...p, stats: { ...p.stats, fixedBaseline: true, rollingMean: 3, segmentId: 1 },
      });
      const stored = [
        fixed(at("a", 100, 2)),
        fixed({ ...at("b", 100, 4), changeAnnotations: { baseline_override: { value: 3, through_result_id: "b", result_count: 2 } } }),
      ];
      const { container } = render(SeriesChart, { props: { points: stored } });
      const caption = container.querySelector(".baseline-caption")?.textContent?.replace(/\s+/g, " ") ?? "";
      expect(caption).toContain("the average of 2 results through sha-b");
    });

    it("labels a stored selected baseline as using later results", () => {
      const fixed = (p: SeriesPoint): SeriesPoint => ({
        ...p, stats: { ...p.stats, fixedBaseline: true, rollingMean: 3, segmentId: 1 },
      });
      const stored = [
        points[0]!,
        fixed({ ...points[1]!, changeAnnotations: { baseline_override: { value: 3, through_result_id: "c", result_count: 2 } } }),
        fixed(points[2]!),
        fixed(points[3]!),
      ];
      const { container } = render(SeriesChart, { props: { points: stored } });
      const caption = container.querySelector(".baseline-caption")?.textContent?.replace(/\s+/g, " ") ?? "";
      expect(caption).toContain("Selected baseline from sha-b (1970-01-01 00:02)");
      expect(caption).toContain("the average of 2 results through sha-c (1970-01-01 00:04)");
      expect(caption).toContain("uses results committed after the point where it starts");
      expect(screen.queryByRole("button", { name: "Select baseline range" })).toBeNull();
    });
  });
});
