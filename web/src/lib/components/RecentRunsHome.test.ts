import { getBenchDB } from "../api/benchdb";
import type { AxiosInstance } from "axios";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { DEFAULT_HOME_QUERY } from "../router";
import RecentRunsHome from "./RecentRunsHome.svelte";

const GET = vi.fn();
vi.mock("../api/client", () => ({
  createBenchDBClient: () => (getBenchDB({ get: GET } as unknown as AxiosInstance)),
}));

const run = (overrides: Record<string, unknown> = {}) => ({
  run_id: "run-a",
  run_reason: "nightly",
  run_tags: { arch: "x86" },
  machine_names: ["m5"],
  batch_count: 1,
  latest_batch_id: "batch-a",
  result_count: 180,
  error_count: 1,
  series_count: 90,
  latest_result_id: "result-a",
  repository: "https://github.com/apache/arrow",
  commit_sha: "abcdef123456",
  first_result_at: "2026-01-01T00:00:00Z",
  last_result_at: "2026-01-02T00:00:00Z",
  commit: {
    hash: "abcdef123456",
    repository: "https://github.com/apache/arrow",
    message: "Improve vector kernel dispatch",
    author_name: "Contributor A",
    author_login: "contributor-a",
    author_avatar: "https://avatars.githubusercontent.com/u/12345?v=4",
    timestamp: "2026-01-02T00:00:00Z",
  },
  ...overrides,
});

beforeEach(() => {
  GET.mockReset();
  window.history.replaceState(null, "", "/");
});

describe("RecentRunsHome", () => {
  it("renders benchmark run triage around commit, author, and machine identity", async () => {
    GET.mockResolvedValueOnce({ status: 200,
      data: {
        repositories: [
          { repository: "https://github.com/apache/arrow" },
          { repository: "https://github.com/apache/arrow-go" },
        ],
        runs: [
          run({
            attention: {
              status: "failure",
              status_reason: "lookback regression detected",
              report_url: "/ci/report?run_ids=run-a&baseline=fork_point",
              summary: {
                compared: 4,
                regressions: 2,
                benchmark_errors: 0,
                missing_baseline: 0,
                not_comparable: 0,
              },
            },
          }),
          run({ run_id: "run-b", error_count: 0 }),
        ],
      },
    });

    render(RecentRunsHome, { props: {} });

    expect(screen.getByText(/loading/i)).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("heading", { name: "Runs" })).toBeInTheDocument());

    expect(screen.getByLabelText("Recent run summary")).toHaveTextContent("2 runs");
    expect(screen.getByText(/360 results/i)).toBeInTheDocument();
    expect(screen.getAllByText(/1 error/i)).not.toHaveLength(0);
    expect(screen.getByText(/1 machine/i)).toBeInTheDocument();
    expect(screen.queryByText(/attention checked/i)).toBeNull();
    const attention = screen.getByRole("region", { name: /^Needs attention/ });
    expect(attention).toHaveTextContent("the newest 2 runs on this page");
    const review = within(attention).getByRole("link", { name: "Review CI report for run run-a" });
    expect(review).toHaveAttribute("href", "/ci/report?run_ids=run-a&baseline=fork_point");
    expect(review).toHaveTextContent("2 regressions");
    expect(screen.getByRole("link", { name: "Open run run-a" })).toHaveAttribute("href", "/runs/run-a");
    expect(screen.getAllByRole("link", { name: "Open batch batch-a" })[0]).toHaveAttribute(
      "href",
      "/batches/batch-a",
    );
    expect(screen.getAllByText("nightly", { selector: ".reason-chip" })).toHaveLength(2);
    expect(screen.getAllByRole("link", { name: "Open commit abcdef12 on GitHub" })[0]).toHaveAttribute(
      "href",
      "https://github.com/apache/arrow/commit/abcdef123456",
    );
    expect(screen.getAllByRole("columnheader").map((th) => th.textContent)).toEqual(["Commit", "Run", "Results", "When"]);
    expect(screen.queryByRole("link", { name: /sample result/i })).toBeNull();
    const when = screen.getAllByRole("time")[0]!;
    expect(when).toHaveAttribute("datetime", "2026-01-02T00:00:00Z");
    expect(when.textContent).toMatch(/ago|yesterday|last/);
  });

  it("loads runs for the URL's project and names the project", async () => {
    GET.mockResolvedValueOnce({ status: 200,
      data: {
        runs: [
          run({
            run_id: "run-arrow-go",
            repository: "https://github.com/apache/arrow-go",
            commit: {
              ...run().commit,
              repository: "https://github.com/apache/arrow-go",
            },
          }),
        ],
      },
    });

    render(RecentRunsHome, {
      props: { query: { ...DEFAULT_HOME_QUERY, repository: "https://github.com/apache/arrow-go" } },
    });

    await waitFor(() => expect(screen.getByRole("heading", { name: "Runs" })).toBeInTheDocument());
    expect(screen.getByText("apache/arrow-go", { selector: ".eyebrow" })).toBeInTheDocument();
    expect(GET).toHaveBeenCalledWith("/api/runs/recent", { params: {
          page_size: 25,
          include_attention: true,
          repository: "https://github.com/apache/arrow-go",
        } });
  });

  it("uses compact production identifiers without losing full link targets", async () => {
    const longRunID = "66f23037065241d6ac22aaeaea96d29b";
    const longBatchID = "66f23037065241d6ac22aaeaea96d29b-1p";
    GET.mockResolvedValueOnce({ status: 200,
      data: {
        runs: [
          run({
            run_id: longRunID,
            latest_batch_id: longBatchID,
            repository: "https://github.com/apache/arrow",
            result_count: 4568,
            series_count: 4568,
            error_count: 0,
          }),
        ],
      },
    });

    render(RecentRunsHome, { props: {} });

    await waitFor(() => expect(screen.getByRole("heading", { name: "Runs" })).toBeInTheDocument());
    expect(screen.getByText("66f230370652…ea96d29b")).toHaveAttribute("title", longRunID);
    expect(screen.getByText("batch 66f230370652…29b-1p")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: `Open run ${longRunID}` })).toHaveAttribute(
      "href",
      `/runs/${longRunID}`,
    );
    expect(screen.getByRole("link", { name: `Open batch ${longBatchID}` })).toHaveAttribute(
      "href",
      `/batches/${longBatchID}`,
    );
  });

  it("adds a project column only when runs span several projects", async () => {
    GET.mockResolvedValueOnce({ status: 200,
      data: {
        runs: [
          run({ run_id: "run-a", run_reason: null, error_count: 0 }),
          run({ run_id: "run-b", run_reason: null, error_count: 0, repository: "https://github.com/apache/arrow-go" }),
        ],
      },
    });
    render(RecentRunsHome, { props: {} });
    await waitFor(() => expect(screen.getByRole("heading", { name: "Runs" })).toBeInTheDocument());
    expect(screen.getAllByRole("columnheader").map((th) => th.textContent)).toEqual(["Commit", "Project", "Run", "Results", "When"]);
    expect(screen.getByText("apache/arrow-go")).toBeInTheDocument();
    expect(screen.queryByText("0 errors")).not.toBeInTheDocument();
    expect(screen.queryByText("nightly")).toBeNull();
  });

  it("says which runs were checked when none need attention", async () => {
    GET.mockResolvedValueOnce({ status: 200, data: { runs: Array.from({ length: 7 }, (_, i) => run({ run_id: `run-${i}` })) } });
    render(RecentRunsHome, { props: {} });
    expect(await screen.findByText("Nothing needs attention in the newest 5 runs on this page.")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: /needs attention/i })).toBeNull();
  });

  it("advances relative times while the page stays open", async () => {
    vi.useFakeTimers({ toFake: ["Date", "setInterval", "clearInterval"] });
    try {
      vi.setSystemTime(new Date("2026-01-02T00:00:20Z"));
      GET.mockResolvedValueOnce({ status: 200, data: { runs: [run()] } });
      render(RecentRunsHome, { props: {} });
      const when = await screen.findByRole("time");
      expect(when).toHaveTextContent("just now");
      await vi.advanceTimersByTimeAsync(2 * 3600 * 1000);
      expect(when).toHaveTextContent("2 hours ago");
    } finally {
      vi.useRealTimers();
    }
  });

  it("shows an empty state", async () => {
    GET.mockResolvedValueOnce({ status: 200,  data: { runs: [] } });
    render(RecentRunsHome, { props: {} });
    await waitFor(() => expect(screen.getByText("No runs yet")).toBeInTheDocument());
  });

  it("shows an error state", async () => {
    GET.mockResolvedValueOnce({ data: { detail: "statement timeout" }, status: 400 });
    render(RecentRunsHome, { props: {} });
    await waitFor(() => expect(screen.getByText(/failed to load recent runs/i)).toBeInTheDocument());
    expect(screen.getByText(/statement timeout/i)).toBeInTheDocument();
  });
});

it("submits a commit URL search and resets pagination", async () => {
  GET.mockResolvedValueOnce({ status: 200,  data: { runs: [], repositories: [], has_more: false } });
  render(RecentRunsHome, { props: { query: { ...DEFAULT_HOME_QUERY, offset: 25 } } });
  await fireEvent.input(screen.getByRole("searchbox"), { target: { value: " commit/abcdef " } });
  await fireEvent.submit(screen.getByRole("search"));
  expect(new URLSearchParams(location.search).get("q")).toBe("commit/abcdef");
  expect(new URLSearchParams(location.search).has("offset")).toBe(false);
});

it("keeps the search and project when paging to older runs", async () => {
  GET.mockResolvedValueOnce({ status: 200,  data: { runs: [run()], repositories: [], has_more: true } });
  render(RecentRunsHome, { props: { query: { repository: "https://github.com/apache/arrow", q: "abcdef", offset: 25 } } });
  await waitFor(() => expect(screen.getByRole("link", { name: "Next" })).toBeInTheDocument());
  expect(GET).toHaveBeenCalledWith("/api/runs/recent", { params: {
    page_size: 25, include_attention: true, repository: "https://github.com/apache/arrow", q: "abcdef", offset: 25,
  } });
  await fireEvent.click(screen.getByRole("link", { name: "Next" }));
  const params = new URLSearchParams(location.search);
  expect(params.get("offset")).toBe("50");
  expect(params.get("q")).toBe("abcdef");
  expect(params.get("repository")).toBe("https://github.com/apache/arrow");
});
