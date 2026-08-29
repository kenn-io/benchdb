import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { DEFAULT_BROWSE_QUERY } from "../router";
import SeriesBrowse from "./SeriesBrowse.svelte";

const GET = vi.fn();
vi.mock("../api/client", () => ({
  createBenchDBClient: () => ({ GET }),
}));

const item = (fp: string, name: string, overrides: Record<string, unknown> = {}) => ({
  benchmark_id: fp,
  name,
  tags: { name },
  repository: "https://github.com/benchdb/demo",
  unit: "s",
  less_is_better: true,
  status: "stable",
  latest_result_id: `${fp}-r`,
  latest_single_value_summary: 1.5,
  latest_single_value_summary_type: "min",
  machine_names: ["m5"],
  latest_commit_sha: "abc1234def",
  latest_commit_timestamp: "2024-01-07T12:00:00Z",
  latest_result_timestamp: "2024-01-07T13:00:00Z",
  point_count: 6,
  preview_tracks: [
    {
      machine_name: "m5",
      points: [
        { commit_timestamp: "2024-01-01T12:00:00Z", value: 1.2, unit: "s" },
        { commit_timestamp: "2024-01-07T12:00:00Z", value: 1.5, unit: "s" },
      ],
    },
  ],
  ...overrides,
});

beforeEach(() => {
  GET.mockReset();
  window.history.replaceState(null, "", "/");
});

describe("SeriesBrowse", () => {
  it("renders rows after loading", async () => {
    GET.mockResolvedValueOnce({ data: { benchmarks: [item("f1", "demo")], next_page_cursor: null } });
    render(SeriesBrowse, { props: { query: DEFAULT_BROWSE_QUERY } });
    expect(screen.getByRole("heading", { name: /loading benchmark series/i })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("link", { name: "demo" })).toBeInTheDocument());
    expect(screen.getByRole("heading", { name: /benchmark series/i })).toBeInTheDocument();
    expect(screen.getByText(/showing 1 loaded series/i)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /result explorer/i })).toHaveAttribute("href", "/results");
    expect(screen.getByRole("group", { name: /series time window/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /load more/i })).toBeNull();
  });

  it("opens the benchmark trend on row click", async () => {
    GET.mockResolvedValueOnce({ data: { benchmarks: [item("f1", "demo")], next_page_cursor: null } });
    render(SeriesBrowse, { props: { query: DEFAULT_BROWSE_QUERY } });
    await waitFor(() => screen.getByRole("link", { name: "demo" }));
    await fireEvent.click(screen.getByRole("link", { name: "demo" }));
    expect(window.location.pathname).toBe("/series/f1");
  });

  it("shows the empty state when nothing matches", async () => {
    GET.mockResolvedValueOnce({ data: { benchmarks: [], next_page_cursor: null } });
    render(SeriesBrowse, { props: { query: { ...DEFAULT_BROWSE_QUERY, q: "nope" } } });
    await waitFor(() => expect(screen.getByText(/no series match/i)).toBeInTheDocument());
    expect(screen.getByRole("region", { name: /no matching benchmark series/i })).toBeInTheDocument();
  });

  it("shows the error state when the load fails", async () => {
    GET.mockResolvedValueOnce({ error: { detail: "boom" } });
    render(SeriesBrowse, { props: { query: DEFAULT_BROWSE_QUERY } });
    await waitFor(() => expect(screen.getByText(/failed to load series/i)).toBeInTheDocument());
    expect(screen.getByRole("alert")).toHaveTextContent("boom");
  });

  it("loads the next page and appends on Load more", async () => {
    GET.mockResolvedValueOnce({ data: { benchmarks: [item("f1", "one")], next_page_cursor: "cur2" } });
    GET.mockResolvedValueOnce({ data: { benchmarks: [item("f2", "two")], next_page_cursor: null } });
    render(SeriesBrowse, { props: { query: DEFAULT_BROWSE_QUERY } });
    await waitFor(() => screen.getByRole("link", { name: "one" }));
    await fireEvent.click(screen.getByRole("button", { name: /load more/i }));
    await waitFor(() => expect(screen.getByRole("link", { name: "two" })).toBeInTheDocument());
    expect(screen.getByRole("link", { name: "one" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /load more/i })).toBeNull();
    const secondCall = GET.mock.calls[1]![1] as { params: { query: { cursor?: string } } };
    expect(secondCall.params.query.cursor).toBe("cur2");
  });

  it("keeps loaded rows and offers retry when Load more fails", async () => {
    GET.mockResolvedValueOnce({ data: { benchmarks: [item("f1", "one")], next_page_cursor: "cur2" } });
    GET.mockResolvedValueOnce({ error: { detail: "boom" } });
    GET.mockResolvedValueOnce({ data: { benchmarks: [item("f2", "two")], next_page_cursor: null } });

    render(SeriesBrowse, { props: { query: DEFAULT_BROWSE_QUERY } });
    await waitFor(() => screen.getByRole("link", { name: "one" }));

    await fireEvent.click(screen.getByRole("button", { name: /load more/i }));
    await waitFor(() => expect(screen.getByText(/failed to load more/i)).toBeInTheDocument());
    // The table survives the pagination failure and the button is the retry.
    expect(screen.getByRole("link", { name: "one" })).toBeInTheDocument();
    const more = screen.getByRole("button", { name: /load more/i });
    expect(more).toBeEnabled();

    await fireEvent.click(more);
    await waitFor(() => expect(screen.getByRole("link", { name: "two" })).toBeInTheDocument());
    expect(screen.queryByText(/failed to load more/i)).toBeNull();
  });

  it("does not wedge Load more when filters change mid-load-more", async () => {
    // Page 1 with a cursor; the load-more request stays pending until after the
    // filter change; the new query's page 1 also has a cursor.
    GET.mockResolvedValueOnce({ data: { benchmarks: [item("f1", "one")], next_page_cursor: "cur2" } });
    let resolveMore: (v: unknown) => void;
    GET.mockImplementationOnce(() => new Promise((r) => { resolveMore = r; }));
    GET.mockResolvedValueOnce({ data: { benchmarks: [item("f3", "three")], next_page_cursor: "cur3" } });

    const { rerender } = render(SeriesBrowse, { props: { query: DEFAULT_BROWSE_QUERY } });
    await waitFor(() => screen.getByRole("link", { name: "one" }));
    await fireEvent.click(screen.getByRole("button", { name: /load more/i }));
    await rerender({ query: { ...DEFAULT_BROWSE_QUERY, q: "x" } });
    await waitFor(() => screen.getByRole("link", { name: "three" }));
    // The stale load-more resolves now: it must neither append nor disable the button.
    resolveMore!({ data: { benchmarks: [item("f2", "two")], next_page_cursor: null } });
    await waitFor(() => expect(screen.getByRole("button", { name: /load more/i })).toBeEnabled());
    expect(screen.queryByRole("link", { name: "two" })).toBeNull();
  });

  it("navigates with updated URL filters when a window preset changes", async () => {
    GET.mockResolvedValue({ data: { benchmarks: [], next_page_cursor: null } });
    window.history.replaceState(null, "", "/series");
    render(SeriesBrowse, { props: { query: DEFAULT_BROWSE_QUERY } });
    await waitFor(() => screen.getByText(/no series match/i));
    await fireEvent.click(screen.getByRole("button", { name: /last 3 months/i }));
    expect(window.location.pathname).toBe("/series");
    expect(window.location.search).toBe("?window=3mo");
  });

  it("uses the primary search and machine controls while keeping repository filtering advanced", async () => {
    GET.mockResolvedValue({ data: { benchmarks: [item("f1", "demo")], next_page_cursor: null } });
    window.history.replaceState(null, "", "/series");
    render(SeriesBrowse, { props: { query: DEFAULT_BROWSE_QUERY } });
    await waitFor(() => screen.getByRole("link", { name: "demo" }));

    expect(screen.getByRole("searchbox", { name: /search benchmarks/i })).toBeInTheDocument();
    const machineSelect = screen.getByRole("combobox", { name: /machine: all machines/i });
    await fireEvent.click(machineSelect);
    await fireEvent.click(screen.getByRole("option", { name: "m5" }));
    expect(window.location.search).toBe("?hardware=m5");

    expect(screen.queryByLabelText(/^repository url$/i)).toBeNull();
    await fireEvent.click(screen.getByRole("button", { name: /advanced filters/i }));
    await fireEvent.input(screen.getByLabelText(/^repository url$/i), {
      target: { value: "https://github.com/apache/arrow " },
    });
    await fireEvent.submit(screen.getByRole("button", { name: /apply advanced filters/i }).closest("form")!);

    expect(window.location.pathname).toBe("/series");
    expect(window.location.search).toBe("?repository=https%3A%2F%2Fgithub.com%2Fapache%2Farrow");
  });

  it("switches from the table to fleet trend cards", async () => {
    GET.mockResolvedValueOnce({ data: { benchmarks: [item("f1", "demo")], next_page_cursor: null } });
    render(SeriesBrowse, { props: { query: DEFAULT_BROWSE_QUERY } });
    await waitFor(() => screen.getByRole("link", { name: "demo" }));

    await fireEvent.click(screen.getByRole("switch", { name: /trend charts/i }));

    expect(screen.getByRole("region", { name: /benchmark trend cards/i })).toBeInTheDocument();
    expect(screen.getByRole("img", { name: /demo fleet trend preview/i })).toBeInTheDocument();
    expect(screen.queryByRole("table")).toBeNull();
    const yAxis = screen.getByRole("combobox", { name: /y-axis: zero baseline/i });
    await fireEvent.click(yAxis);
    await fireEvent.click(screen.getByRole("option", { name: /observed range/i }));
    expect(screen.getByRole("combobox", { name: /y-axis: observed range/i })).toBeInTheDocument();
  });

  it("shows active filters and can clear them", async () => {
    GET.mockResolvedValue({ data: { benchmarks: [item("f1", "demo")], next_page_cursor: null } });
    render(SeriesBrowse, {
      props: {
        query: {
          q: "demo",
          hardware: "m5",
          repository: "https://github.com/benchdb/demo",
          window: "30d",
        },
      },
    });
    await waitFor(() => screen.getByRole("link", { name: "demo" }));
    expect(screen.getByRole("group", { name: /active filters/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /remove query filter demo/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /remove machine filter m5/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /remove window filter last 30 days/i })).toBeInTheDocument();
    await fireEvent.click(screen.getByRole("button", { name: /clear filters/i }));
    expect(window.location.pathname).toBe("/series");
    expect(window.location.search).toBe("");
  });

  it("removes individual active filters without dropping the others", async () => {
    GET.mockResolvedValue({ data: { benchmarks: [item("f1", "demo")], next_page_cursor: null } });
    render(SeriesBrowse, {
      props: {
        query: {
          q: "demo",
          hardware: "m5",
          repository: "https://github.com/benchdb/demo",
          window: "30d",
        },
      },
    });
    await waitFor(() => screen.getByRole("link", { name: "demo" }));

    await fireEvent.click(screen.getByRole("button", { name: /remove machine filter m5/i }));

    expect(window.location.pathname).toBe("/series");
    const params = new URLSearchParams(window.location.search);
    expect(params.get("q")).toBe("demo");
    expect(params.get("hardware")).toBeNull();
    expect(params.get("repository")).toBe("https://github.com/benchdb/demo");
    expect(params.get("window")).toBe("30d");
  });

  it("sorts the visible rows when a header is clicked", async () => {
    GET.mockResolvedValueOnce({
      data: { benchmarks: [item("f1", "bbb"), item("f2", "aaa")], next_page_cursor: null },
    });
    render(SeriesBrowse, { props: { query: DEFAULT_BROWSE_QUERY } });
    await waitFor(() => screen.getByRole("link", { name: "bbb" }));
    await fireEvent.click(screen.getByRole("button", { name: "Sort by benchmark" }));
    expect(screen.getByText(/sorting applies to loaded rows/i)).toBeInTheDocument();
    // BrowseTable marks each whole row as role="link" too; scope to the name
    // anchors so the assertion reads the sorted benchmark labels alone.
    const links = within(screen.getByRole("table"))
      .getAllByRole("link")
      .filter((el) => el.tagName === "A")
      .map((a) => a.textContent);
    expect(links).toEqual(["aaa", "bbb"]);
  });
});
