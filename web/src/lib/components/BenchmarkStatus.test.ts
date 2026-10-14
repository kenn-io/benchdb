import { render, screen } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";

import type { BrowseRow } from "../browse/transform";
import BenchmarkStatus from "./BenchmarkStatus.svelte";

const row = (over: Partial<BrowseRow>): BrowseRow => ({
  benchmarkId: "b1", name: "tpch-q1", repository: "", paramsText: "", machineNames: ["m5"],
  latestSVS: 1, unit: "s", svsText: "1 s", pointCount: 9, status: "regressed",
  changePercent: null, changeMachine: null,
  commitSha: "abc1234", commitTimestampMs: 0, commitDateText: "", previewTracks: [],
  ...over,
});

describe("BenchmarkStatus", () => {
  it("shows how far a regression moved, and says it in words", () => {
    const { container } = render(BenchmarkStatus, { props: { row: row({ changePercent: 12.04, changeMachine: "m5" }) } });
    expect(screen.getByText("regressed")).toBeInTheDocument();
    expect(screen.getByText("+12.0%")).toBeInTheDocument();
    expect(container).toHaveTextContent("latest 12.0% above its history mean on m5");
    expect(screen.queryByText("m5")).toBeNull();
  });

  it("names the machine that set the status when the benchmark runs on several", () => {
    render(BenchmarkStatus, { props: { row: row({
      machineNames: ["m5", "m7"], status: "improved", changePercent: -8.31, changeMachine: "m7",
    }) } });
    expect(screen.getByText("m7")).toBeInTheDocument();
    expect(screen.getByText(/latest 8.3% below its history mean on m7/)).toBeInTheDocument();
  });

  it("shows only the status when there is no change to report", () => {
    const { container } = render(BenchmarkStatus, { props: { row: row({ status: "insufficient" }) } });
    expect(container).toHaveTextContent(/^\s*insufficient\s*$/);
  });

  it("says a change that rounds to zero is at the mean", () => {
    const { container } = render(BenchmarkStatus, { props: { row: row({ status: "stable", changePercent: 0.02, changeMachine: "m5" }) } });
    expect(container).toHaveTextContent("latest at its history mean on m5");
  });
});
