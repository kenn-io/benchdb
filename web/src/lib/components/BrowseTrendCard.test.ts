import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { getBenchDB } from "../api/benchdb";
import type { AxiosInstance } from "axios";

import type { BrowseRow } from "../browse/transform";
import BrowseTrendCard from "./BrowseTrendCard.svelte";

const GET = vi.fn();
const client = getBenchDB({ get: GET } as unknown as AxiosInstance);
vi.mock("../api/client", () => ({ createBenchDBClient: () => client }));
beforeEach(() => { GET.mockReset(); });

const row: BrowseRow = {
  benchmarkId: "bench-1",
  name: "daily-usage",
  repository: "https://github.com/benchdb/demo",
  paramsText: "scale=large",
  machineNames: ["m5"],
  latestSVS: 12,
  unit: "s",
  svsText: "12 s",
  pointCount: 3,
  status: "stable",
  commitSha: "abc1234",
  commitTimestampMs: Date.parse("2024-01-11T00:00:00Z"),
  commitDateText: "Jan 11, 2024",
  previewTracks: [
    {
      machineName: "m5",
      points: [
        { chartMs: Date.parse("2024-01-01T00:00:00Z"), resultId: "r3", value: 10, unit: "s" },
        { chartMs: Date.parse("2024-01-02T00:00:00Z"), resultId: "r4", value: 11, unit: "s" },
        { chartMs: Date.parse("2024-01-11T00:00:00Z"), resultId: "r5", value: 12, unit: "s" },
      ],
    },
  ],
};

describe("BrowseTrendCard", () => {
  it("loads omitted history and keeps the selected machine and time window", async () => {
    const samples = Array.from({ length: 25 }, (_, index) => ({
      commit_timestamp: `2024-01-${String(index + 1).padStart(2, "0")}T12:00:00Z`,
      result_timestamp: `2024-01-${String(index + 1).padStart(2, "0")}T12:00:00Z`,
      single_value_summary: index + 1,
      unit: "s", change_annotations: {}, zscorestats: null,
    }));
    GET.mockImplementation(async (url: string) => {
      expect(url).toBe("/api/benchmarks/bench-1");
      return { status: 200, data: {
        benchmark_id: "bench-1", name: row.name, tags: {}, repository: "",
        unit: "s", less_is_better: true,
        tracks: [
          { machine_name: "m5", segments: [
            { history_fingerprint: "new", samples: samples.slice(10) },
            { history_fingerprint: "old", samples: samples.slice(0, 10) },
          ] },
          { machine_name: "other-machine", segments: [{ samples }] },
        ],
      } };
    });
    const { container } = render(BrowseTrendCard, { props: {
      row: { ...row, pointCount: 24 },
      timeRange: { min: Date.parse("2024-01-02T00:00:00Z"), max: Date.parse("2024-02-01T00:00:00Z") },
    } });

    expect(screen.getByText("Loading history…")).toBeInTheDocument();
    await waitFor(() => expect(container.querySelectorAll(".point-mark")).toHaveLength(24));
    const titles = [...container.querySelectorAll(".point-hit title")].map((title) => title.textContent);
    expect(titles[0]).toMatch(/m5.*2 s/);
    expect(titles.at(-1)).toMatch(/m5.*25 s/);
    expect(screen.queryByText("other-machine")).toBeNull();
  });

  it("shows the API's latest result when two contexts share the latest commit", async () => {
    const sample = (id: string, day: number, value: number) => ({
      benchmark_result_id: id,
      commit_timestamp: `2024-01-${String(day).padStart(2, "0")}T12:00:00Z`,
      result_timestamp: `2024-01-${String(day).padStart(2, "0")}T12:00:00Z`,
      single_value_summary: value,
      unit: "s", change_annotations: {}, zscorestats: null,
    });
    // Segment order (a, then z) disagrees with result-ID order at the shared
    // latest commit, where the API reports result r9.
    GET.mockResolvedValue({ status: 200, data: {
      benchmark_id: "bench-1", name: row.name, tags: {}, repository: "",
      unit: "s", less_is_better: true,
      tracks: [{ machine_name: "m5", segments: [
        { history_fingerprint: "a", samples: [sample("r1", 1, 10), sample("r9", 5, 30)] },
        { history_fingerprint: "z", samples: [sample("r2", 2, 11), sample("r3", 5, 20)] },
      ] }],
    } });
    const { container } = render(BrowseTrendCard, { props: { row: { ...row, pointCount: 4 } } });

    await waitFor(() => expect(container.querySelectorAll(".point-hit")).toHaveLength(4));
    expect(within(container.querySelector("footer")!).getByText("m5").parentElement).toHaveTextContent(/30 s/);
    expect(screen.getByRole("button", { name: /open trend daily-usage/i })).toHaveAccessibleName(/m5: latest 30 s/);
    const latest = container.querySelector(".point-mark.latest")!;
    const highest = Math.min(...[...container.querySelectorAll(".point-hit")].map((hit) => Number(hit.getAttribute("cy"))));
    expect(Number(latest.getAttribute("cy"))).toBe(highest);
  });

  it("shows each machine's latest value inside the selected window", async () => {
    const sample = (id: string, day: number, value: number) => ({
      benchmark_result_id: id,
      commit_timestamp: `2024-01-${String(day).padStart(2, "0")}T12:00:00Z`,
      result_timestamp: `2024-01-${String(day).padStart(2, "0")}T12:00:00Z`,
      single_value_summary: value,
      unit: "s", change_annotations: {}, zscorestats: null,
    });
    // A result newer than the window arrived after the list loaded.
    GET.mockResolvedValue({ status: 200, data: {
      benchmark_id: "bench-1", name: row.name, tags: {}, repository: "",
      unit: "s", less_is_better: true,
      tracks: [{ machine_name: "m5", segments: [{ history_fingerprint: "a", samples: [
        sample("r1", 1, 10), sample("r2", 2, 11), sample("r3", 11, 12), sample("r4", 20, 99),
      ] }] }],
    } });
    const { container } = render(BrowseTrendCard, { props: {
      row: { ...row, pointCount: 4 },
      timeRange: { min: Date.parse("2024-01-01T00:00:00Z"), max: Date.parse("2024-01-15T00:00:00Z") },
    } });

    await waitFor(() => expect(container.querySelector(".point-mark.latest")).not.toBeNull());
    const legend = within(container.querySelector("footer")!).getByText("m5").parentElement!;
    expect(legend).toHaveTextContent(/12 s/);
    expect(legend).not.toHaveTextContent(/99 s/);
    expect(screen.getByRole("button", { name: /open trend daily-usage/i })).toHaveAccessibleName(/m5: latest 12 s/);
  });

  it("waits to load omitted history until the card nears the viewport", async () => {
    let reveal: () => void = () => {};
    let observedMargin = "";
    let observedRoot: Element | Document | null | undefined;
    class OffscreenObserver {
      constructor(callback: IntersectionObserverCallback, options?: IntersectionObserverInit) {
        observedMargin = options?.rootMargin ?? "";
        observedRoot = options?.root;
        reveal = () => callback([{ isIntersecting: true } as IntersectionObserverEntry], this as never);
      }
      observe(): void {}
      disconnect(): void {}
    }
    const original = globalThis.IntersectionObserver;
    globalThis.IntersectionObserver = OffscreenObserver as unknown as typeof IntersectionObserver;
    try {
      GET.mockResolvedValue({ status: 500, data: { detail: "history unavailable" } });
      const scroller = document.body.appendChild(document.createElement("div"));
      scroller.className = "app-content";
      render(BrowseTrendCard, { target: scroller, props: { row: { ...row, pointCount: 24 } } });
      expect(screen.getByText("Loading history…")).toBeInTheDocument();
      expect(observedMargin).toBe("600px 0px");
      expect(observedRoot).toBe(scroller);
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(GET).not.toHaveBeenCalled();

      reveal();
      await waitFor(() => expect(GET).toHaveBeenCalledTimes(1));
      expect(GET.mock.calls[0]![0]).toBe("/api/benchmarks/bench-1");
    } finally {
      globalThis.IntersectionObserver = original;
    }
  });

  it("shows a history failure instead of presenting the truncated preview as complete", async () => {
    GET.mockResolvedValue({ status: 500, data: { detail: "history unavailable" } });
    const { container } = render(BrowseTrendCard, { props: { row: { ...row, pointCount: 30 } } });
    expect(await screen.findByRole("alert")).toHaveTextContent(/failed to load/i);
    expect(container.querySelector("svg")).toBeNull();
  });

  it("fits intraday data inside a wide selected window and labels the times", () => {
    const { container } = render(BrowseTrendCard, { props: {
      row: { ...row, previewTracks: [{ machineName: "m5", points: [
        { chartMs: Date.parse("2024-01-11T12:00:00Z"), resultId: "r6", value: 10, unit: "s" },
        { chartMs: Date.parse("2024-01-11T13:00:00Z"), resultId: "r7", value: 11, unit: "s" },
        { chartMs: Date.parse("2024-01-11T14:00:00Z"), resultId: "r8", value: 12, unit: "s" },
      ] }] },
      timeRange: { min: Date.parse("2023-11-01T00:00:00Z"), max: Date.parse("2024-02-01T00:00:00Z") },
    } });
    const marks = container.querySelectorAll(".point-mark");
    const axis = container.querySelector(".axis")!;
    expect(marks[0]!.getAttribute("cx")).toBe(axis.getAttribute("x1"));
    expect(marks[2]!.getAttribute("cx")).toBe(axis.getAttribute("x2"));
    const labels = container.querySelectorAll(".axis-label");
    expect(labels[0]!.textContent).not.toBe(labels[1]!.textContent);
  });

  it("spaces preview points by calendar time, labels the range, and explains hovered points", async () => {
    const onopen = vi.fn();
    const { container } = render(BrowseTrendCard, { props: { row, onopen } });

    const path = container.querySelector("path")?.getAttribute("d");
    const xValues = [...(path?.matchAll(/[ML]([\d.]+),/g) ?? [])].map((match) => Number(match[1]));
    expect(xValues).toHaveLength(3);
    expect(xValues[0]).toBe(Number(container.querySelector(".axis")!.getAttribute("x1")));
    expect(xValues[1]! - xValues[0]!).toBeLessThan((xValues[2]! - xValues[1]!) / 5);

    const dateFormat = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" });
    expect([...container.querySelectorAll(".axis-label")].map((label) => label.textContent)).toEqual([
      dateFormat.format(Date.parse("2024-01-01T00:00:00Z")),
      dateFormat.format(Date.parse("2024-01-11T00:00:00Z")),
    ]);

    await fireEvent.pointerEnter(container.querySelector(".point-hit")!);
    expect(screen.getByRole("tooltip")).toHaveTextContent(/m5.*10 s/i);

    await fireEvent.click(screen.getByRole("button", { name: /open trend daily-usage/i }));
    expect(onopen).toHaveBeenCalledWith(row);
  });

  it("can scale its preview from the observed minimum", async () => {
    const { container, rerender } = render(BrowseTrendCard, { props: { row } });
    const marks = container.querySelectorAll(".point-mark");
    expect(Number(marks[0]!.getAttribute("cy")) - Number(marks[2]!.getAttribute("cy"))).toBeGreaterThan(100);
    const observedPath = container.querySelector("path")?.getAttribute("d");

    await rerender({ row, zeroBased: true });

    expect(container.querySelector("path")?.getAttribute("d")).not.toBe(observedPath);
  });

  it("labels the value axis at round values in the benchmark's unit", () => {
    const { container } = render(BrowseTrendCard, { props: { row } });
    expect([...container.querySelectorAll(".value-label")].map((label) => label.textContent))
      .toEqual(["10 s", "11 s", "12 s"]);
  });

  it("shows each machine's latest value and summarizes the chart for screen readers", () => {
    const twoMachines: BrowseRow = { ...row, machineNames: ["m5", "m7"], previewTracks: [
      row.previewTracks[0]!,
      { machineName: "m7", points: [
        { chartMs: Date.parse("2024-01-01T00:00:00Z"), resultId: "r9", value: 20, unit: "s" },
        { chartMs: Date.parse("2024-01-10T00:00:00Z"), resultId: "r10", value: 21, unit: "s" },
      ] },
    ] };
    render(BrowseTrendCard, { props: { row: twoMachines } });

    const legend = screen.getByRole("contentinfo");
    expect(within(legend).getByText("m5").parentElement).toHaveTextContent(/m5\s*12 s/);
    expect(within(legend).getByText("m7").parentElement).toHaveTextContent(/m7\s*21 s/);
    expect(screen.getByRole("button", { name: /open trend daily-usage/i })).toHaveAccessibleName(
      "Open trend daily-usage. m5: latest 12 s, 3 results from 10 s to 12 s. " +
      "m7: latest 21 s, 2 results from 20 s to 21 s",
    );
  });

  it("keeps a machine's color when other machines are filtered out", () => {
    const twoMachines: BrowseRow = { ...row, machineNames: ["a-machine", "m5"], previewTracks: [
      { machineName: "a-machine", points: row.previewTracks[0]!.points },
      row.previewTracks[0]!,
    ] };
    const swatch = (container: HTMLElement) =>
      (within(container.querySelector("footer")!).getByText("m5").previousElementSibling as HTMLElement).style.background;

    const both = render(BrowseTrendCard, { props: { row: twoMachines } });
    const withOthers = swatch(both.container);
    both.unmount();
    const alone = render(BrowseTrendCard, { props: { row } });

    expect(withOthers).not.toBe("");
    expect(swatch(alone.container)).toBe(withOthers);
  });

  it("draws only the line and the latest point when points are too dense for dots", () => {
    const start = Date.parse("2024-01-01T00:00:00Z");
    const dense: BrowseRow = { ...row, pointCount: 200, previewTracks: [{ machineName: "m5", points:
      Array.from({ length: 200 }, (_, index) => ({ chartMs: start + index * 3_600_000, resultId: "r11", value: 10 + (index % 5), unit: "s" })),
    }] };
    const { container } = render(BrowseTrendCard, { props: { row: dense } });

    const marks = container.querySelectorAll(".point-mark");
    expect(marks).toHaveLength(1);
    expect(marks[0]).toHaveClass("latest");
    expect(marks[0]!.getAttribute("cx")).toBe(container.querySelector(".axis")!.getAttribute("x2"));
  });

  it("keeps the tooltip inside the chart at its top and side edges", async () => {
    // jsdom has no layout; give the chart and tooltip the sizes a browser would.
    const CHART_WIDTH = 300;
    const TOOLTIP_WIDTH = 200;
    const TOOLTIP_HEIGHT = 30;
    const width = vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(function (this: HTMLElement) {
      if (this.classList.contains("chart-button")) return CHART_WIDTH;
      return this.classList.contains("chart-tooltip") ? TOOLTIP_WIDTH : 0;
    });
    const height = vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockImplementation(function (this: HTMLElement) {
      return this.classList.contains("chart-tooltip") ? TOOLTIP_HEIGHT : 0;
    });
    try {
      const { container } = render(BrowseTrendCard, { props: { row } });
      expect(container.querySelector("svg")).toHaveAttribute("width", String(CHART_WIDTH));
      const hits = [...container.querySelectorAll(".point-hit")];
      // The first point sits at the left edge; the latest and highest at the top right.
      for (const hit of [hits[0]!, hits[hits.length - 1]!]) {
        await fireEvent.pointerEnter(hit);
        const tooltip = screen.getByRole("tooltip");
        expect(tooltip.style.left).toMatch(/px$/);
        expect(tooltip.style.top).toMatch(/px$/);
        const left = Number.parseFloat(tooltip.style.left);
        const top = Number.parseFloat(tooltip.style.top);
        expect(left).toBeGreaterThanOrEqual(0);
        expect(left + TOOLTIP_WIDTH).toBeLessThanOrEqual(CHART_WIDTH);
        expect(top).toBeGreaterThanOrEqual(0);
        expect(top + TOOLTIP_HEIGHT).toBeLessThanOrEqual(150);
        await fireEvent.pointerLeave(hit);
      }
    } finally {
      width.mockRestore();
      height.mockRestore();
    }
  });

  it("does not plot preview points with mixed units", () => {
    const mixed = structuredClone(row);
    mixed.previewTracks[0]!.points[0]!.unit = "B";

    const { container } = render(BrowseTrendCard, { props: { row: mixed } });

    expect(screen.getByText("Preview unavailable: mixed units")).toBeInTheDocument();
    expect(container.querySelector("svg")).toBeNull();
    expect(screen.getByRole("button", { name: /open trend daily-usage/i })).toHaveAccessibleName(
      "Open trend daily-usage. Preview unavailable: mixed units",
    );
  });
});
