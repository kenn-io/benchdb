import { fireEvent, render, screen } from "@testing-library/svelte";
import { describe, expect, it, vi } from "vitest";

import type { BrowseRow } from "../browse/transform";
import BrowseTrendCard from "./BrowseTrendCard.svelte";

const row: BrowseRow = {
  benchmarkId: "bench-1",
  name: "daily-usage",
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
        { chartMs: Date.parse("2024-01-01T00:00:00Z"), value: 10 },
        { chartMs: Date.parse("2024-01-02T00:00:00Z"), value: 11 },
        { chartMs: Date.parse("2024-01-11T00:00:00Z"), value: 12 },
      ],
    },
  ],
};

describe("BrowseTrendCard", () => {
  it("spaces preview points by calendar time and opens the full trend", async () => {
    const onopen = vi.fn();
    const { container } = render(BrowseTrendCard, { props: { row, onopen } });

    const path = container.querySelector("path")?.getAttribute("d");
    expect(path).toMatch(/^M8\.00,/);
    const xValues = [...(path?.matchAll(/[ML]([\d.]+),/g) ?? [])].map((match) => Number(match[1]));
    expect(xValues).toHaveLength(3);
    expect(xValues[1]! - xValues[0]!).toBeLessThan((xValues[2]! - xValues[1]!) / 5);

    await fireEvent.click(screen.getByRole("button", { name: /open trend daily-usage/i }));
    expect(onopen).toHaveBeenCalledWith(row);
  });
});
