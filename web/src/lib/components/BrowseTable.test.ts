import { fireEvent, render, screen, within } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { toBrowseRows } from "../browse/transform";
import BrowseTable from "./BrowseTable.svelte";

// Anchor clicks that the component does not handle fall through to the native
// default action; jsdom does not implement navigation and logs to stderr.
// Swallow the default here so output stays clean while recording the component's
// own decision. When the component intercepts a click it calls
// stopPropagation(), so this document-level listener never runs and the value
// stays at its "prevented" default. When the component declines (no onopen, or a
// modified click) the event bubbles here with defaultPrevented still false,
// which records the native fall-through before we swallow it. Hence the reset is
// true (assume intercepted) and only an observed bubble flips it to false.
let componentPreventedDefault = true;

function swallowAnchorNavigation(e: Event) {
  componentPreventedDefault = e.defaultPrevented;
  e.preventDefault();
}

beforeEach(() => {
  componentPreventedDefault = true;
  document.addEventListener("click", swallowAnchorNavigation);
});
afterEach(() => document.removeEventListener("click", swallowAnchorNavigation));

const rows = toBrowseRows(
  [
    {
      benchmark_id: "b1",
      name: "tpch-q1",
      tags: { name: "tpch-q1", scale: "sf10" },
      repository: "https://github.com/benchdb/demo",
      unit: "s",
      less_is_better: true,
      status: "regressed",
      change_percent: null,
      change_machine: null,
      latest_result_id: "r9",
      latest_single_value_summary: 1.2,
      latest_single_value_summary_type: "min",
      machine_names: ["m5", "m7"],
      latest_commit_sha: "a1b2c3d4e5",
      latest_commit_timestamp: "2024-01-07T12:00:00Z",
      latest_result_timestamp: "2024-01-07T13:00:00Z",
      point_count: 8,
      preview_tracks: [],
    },
  ],
  "en-US",
);

describe("BrowseTable", () => {
  it("summarizes value, history, and commit under the name for narrow screens", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    const onopen = vi.fn();
    const { container } = render(BrowseTable, { props: { rows, onopen } });
    const summary = container.querySelector<HTMLElement>(".narrow-meta")!;
    expect(summary).toHaveTextContent(/1\.2 s\s*· 8 points · 2 machines · a1b2c3d · /);
    expect(summary.querySelector(".mono")).toHaveTextContent("a1b2c3d");

    // The summary's value keeps the exact-value copy action, and copying does
    // not open the benchmark.
    await fireEvent.click(within(summary).getByRole("button", { name: /exact value 1\.2/ }));
    expect(writeText).toHaveBeenCalledWith("1.2");
    expect(onopen).not.toHaveBeenCalled();
  });


  it("keeps benchmark rows as table rows with one link to open them", () => {
    render(BrowseTable, { props: { rows } });
    const row = screen.getByRole("row", { name: /tpch-q1/ });
    expect(within(row).getByRole("cell", { name: /tpch-q1/ })).toBeInTheDocument();
    expect(screen.getAllByRole("link").filter((link) => /tpch-q1/.test(link.textContent ?? ""))).toHaveLength(1);
  });

  it("renders one row with identity, value, status, and trend link", () => {
    const { container } = render(BrowseTable, { props: { rows } });
    expect(screen.getByRole("link", { name: "tpch-q1" })).toHaveAttribute("href", "/benchmarks/b1");
    expect(screen.getByText("scale=sf10")).toBeInTheDocument();
    expect(container.querySelector(".num-cell")).toHaveTextContent("1.2 s");
    expect(screen.getByText("regressed")).toBeInTheDocument();
    expect(container.querySelector(".commit-cell")).toHaveTextContent(/^a1b2c3d · /);
  });

  it("uses the shared data-table and stacked-table treatment", () => {
    const { container } = render(BrowseTable, { props: { rows } });
    expect(container.querySelector(".table-panel > table.data-table.stacked-table.browse-table")).not.toBeNull();
  });

  it("groups multi-part mobile cell content into one value item", () => {
    render(BrowseTable, { props: { rows } });
    const nameCell = screen.getByRole("link", { name: "tpch-q1" }).closest("td");
    expect(nameCell?.querySelector(".identity-stack")).not.toBeNull();
    expect(nameCell?.querySelector(".identity-stack .metadata-line")).toHaveTextContent("scale=sf10");
  });

  it("keeps long machine names readable without exposing environment payloads", () => {
    const longRows = toBrowseRows(
      [
        {
          benchmark_id: "long-benchmark",
          name: "benchmark-with-a-very-long-name/including/repository-ish/path/components/and-extra-identifiers",
          tags: {
            name: "benchmark-with-a-very-long-name/including/repository-ish/path/components/and-extra-identifiers",
            parameter_set: "dataset=parquet-wide-1tb-and-filter=very-long-expression",
            codec: "zstd-level-19-with-long-option-name",
          },
          repository: "https://github.com/benchdb/demo",
          unit: "s",
          less_is_better: true,
          status: "stable",
          change_percent: null,
          change_machine: null,
          latest_result_id: "long-r",
          latest_single_value_summary: 1234.567,
          latest_single_value_summary_type: "min",
          machine_names: ["production-runner-us-east-1-c7i-metal-24xl-with-extra-suffix"],
          latest_commit_sha: "0123456789abcdef",
          latest_commit_timestamp: "2024-01-07T12:00:00Z",
          latest_result_timestamp: "2024-01-07T13:00:00Z",
          point_count: 42,
          preview_tracks: [],
        },
      ],
      "en-US",
    );

    render(BrowseTable, { props: { rows: longRows } });

    expect(screen.getByRole("link", { name: /^benchmark-with-a-very-long-name/ })).toHaveClass("row-primary-link");
    expect(screen.getByText("1 machine").closest("td")).toHaveClass("history-cell");
    expect(screen.getByText("stable").closest("td")).toHaveClass("status-cell");
  });

  it("opens a row on link click without a page load", async () => {
    const onopen = vi.fn();
    render(BrowseTable, { props: { rows, onopen } });
    await fireEvent.click(screen.getByRole("link", { name: "tpch-q1" }));
    expect(onopen).toHaveBeenCalledWith(rows[0]);
    expect(componentPreventedDefault).toBe(true);
  });

  it("falls back to native link navigation when onopen is absent", async () => {
    render(BrowseTable, { props: { rows } });
    await fireEvent.click(screen.getByRole("link", { name: "tpch-q1" }));
    expect(componentPreventedDefault).toBe(false);
  });

  it("opens exactly once per link click (no tr double-fire)", async () => {
    const onopen = vi.fn();
    render(BrowseTable, { props: { rows, onopen } });
    await fireEvent.click(screen.getByRole("link", { name: "tpch-q1" }));
    expect(onopen).toHaveBeenCalledTimes(1);
  });

  it("leaves modified link clicks to the browser", async () => {
    const onopen = vi.fn();
    render(BrowseTable, { props: { rows, onopen } });
    await fireEvent.click(screen.getByRole("link", { name: "tpch-q1" }), { metaKey: true });
    expect(onopen).not.toHaveBeenCalled();
  });

  it("reports header clicks for sortable columns only", async () => {
    const onsort = vi.fn();
    render(BrowseTable, { props: { rows, onsort } });
    await fireEvent.click(screen.getByRole("button", { name: "Sort by benchmark" }));
    expect(onsort).toHaveBeenCalledWith("name");
    expect(screen.queryByRole("button", { name: /^status$/ })).toBeNull();
  });

  it("marks the active sort header for assistive technology", () => {
    render(BrowseTable, { props: { rows, sort: { key: "name", dir: "desc" } } });
    const button = screen.getByRole("button", { name: "Sort by benchmark" });
    expect(button).toHaveClass("active");
    expect(button).toHaveAttribute("aria-pressed", "true");
    expect(button.closest("th")).toHaveAttribute("aria-sort", "descending");
    expect(screen.getByRole("button", { name: "Sort by last value" }).closest("th")).not.toHaveAttribute(
      "aria-sort",
    );
  });
});
