import { fireEvent, render, screen, waitFor } from "@testing-library/svelte";
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
        { chartMs: Date.parse("2024-01-01T00:00:00Z"), value: 10, unit: "s" },
        { chartMs: Date.parse("2024-01-02T00:00:00Z"), value: 11, unit: "s" },
        { chartMs: Date.parse("2024-01-11T00:00:00Z"), value: 12, unit: "s" },
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

  it("waits to load omitted history until the card nears the viewport", async () => {
    let reveal: () => void = () => {};
    let observedMargin = "";
    class OffscreenObserver {
      constructor(callback: IntersectionObserverCallback, options?: IntersectionObserverInit) {
        observedMargin = options?.rootMargin ?? "";
        reveal = () => callback([{ isIntersecting: true } as IntersectionObserverEntry], this as never);
      }
      observe(): void {}
      disconnect(): void {}
    }
    const original = globalThis.IntersectionObserver;
    globalThis.IntersectionObserver = OffscreenObserver as unknown as typeof IntersectionObserver;
    try {
      GET.mockResolvedValue({ status: 500, data: { detail: "history unavailable" } });
      render(BrowseTrendCard, { props: { row: { ...row, pointCount: 24 } } });
      expect(screen.getByText("Loading history…")).toBeInTheDocument();
      expect(observedMargin).toBe("600px 0px");
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
        { chartMs: Date.parse("2024-01-11T12:00:00Z"), value: 10, unit: "s" },
        { chartMs: Date.parse("2024-01-11T13:00:00Z"), value: 11, unit: "s" },
        { chartMs: Date.parse("2024-01-11T14:00:00Z"), value: 12, unit: "s" },
      ] }] },
      timeRange: { min: Date.parse("2023-11-01T00:00:00Z"), max: Date.parse("2024-02-01T00:00:00Z") },
    } });
    const marks = container.querySelectorAll(".point-mark");
    expect(Number(marks[0]!.getAttribute("cx"))).toBe(8);
    expect(Number(marks[2]!.getAttribute("cx"))).toBe(512);
    const labels = container.querySelectorAll(".axis-label");
    expect(labels[0]!.textContent).not.toBe(labels[1]!.textContent);
  });

  it("spaces preview points by calendar time, labels the range, and explains hovered points", async () => {
    const onopen = vi.fn();
    const { container } = render(BrowseTrendCard, { props: { row, onopen } });

    const path = container.querySelector("path")?.getAttribute("d");
    expect(path).toMatch(/^M8\.00,/);
    const xValues = [...(path?.matchAll(/[ML]([\d.]+),/g) ?? [])].map((match) => Number(match[1]));
    expect(xValues).toHaveLength(3);
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

  it("does not plot preview points with mixed units", () => {
    const mixed = structuredClone(row);
    mixed.previewTracks[0]!.points[0]!.unit = "B";

    const { container } = render(BrowseTrendCard, { props: { row: mixed } });

    expect(screen.getByText("Preview unavailable: mixed units")).toBeInTheDocument();
    expect(container.querySelector("svg")).toBeNull();
  });
});
