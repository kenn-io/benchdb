import { fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { components } from "../api/schema";
import ResultPage from "./ResultPage.svelte";

const GET = vi.fn();
const PUT = vi.fn();
const DELETE = vi.fn();
vi.mock("../api/client", () => ({
  createBenchDBClient: () => ({ GET, PUT, DELETE }),
}));
vi.mock("./SeriesChart.svelte", async () => await import("./SeriesChart.stub.svelte"));

type ResultDetail = components["schemas"]["ResultDetail"];
type HistorySample = components["schemas"]["HistorySample"];

const detail: ResultDetail = {
  id: "r1",
  benchmark_id: "b1",
  batch_id: null,
  run_id: "run1",
  run_reason: "commit",
  run_tags: {},
  tags: { name: "demo-benchmark", scale: "sf10" },
  context: { compiler: "gcc" },
  info: { suite: "arrow", benchmark_language: "C++" },
  hardware: { id: "h1", type: "machine", name: "m5", hash: "hw1" },
  commit: {
    id: "c1",
    message: "tune",
    repository: "https://github.com/benchdb/demo",
    sha: "abc1234def",
    timestamp: "2024-01-07T12:00:00Z",
  },
  commit_repo_url: "https://github.com/benchdb/demo",
  unit: "s",
  less_is_better: true,
  single_value_summary: 1.5,
  single_value_summary_type: "min",
  iterations: 3,
  data: [1.4, 1.5, 1.6],
  times: [0.1, 0.2, 0.3],
  time_unit: "s",
  error: null,
  stats: { min: 1.4, max: 1.6, mean: 1.5, median: 1.5, q1: null, q3: null, stdev: 0.1, iqr: null },
  optional_benchmark_info: { owner: "perf" },
  validation: { success: true, validator: "pandas.testing" },
  change_annotations: { begins_distribution_change: false, note: "checked" },
  history_fingerprint: "fp1",
  timestamp: "2024-01-07T13:00:00Z",
};

const signedInCapabilities = {
  signed_in: true,
  auth_disabled: false,
  can_write_results: true,
};

const readOnlyCapabilities = {
  signed_in: false,
  auth_disabled: false,
  can_write_results: false,
};

const authDisabledCapabilities = {
  signed_in: false,
  auth_disabled: true,
  can_write_results: true,
};

const historySample = (
  id: string,
  sha: string,
  timestamp: string,
  value: number,
): HistorySample => ({
  benchmark_result_id: id,
  commit_hash: sha,
  commit_message: id === "r1" ? "tune" : "before",
  commit_repository: "https://github.com/benchdb/demo",
  commit_timestamp: timestamp,
  data: null,
  hardware_hash: "hw1",
  mean: value,
  result_timestamp: timestamp,
  single_value_summary: value,
  single_value_summary_type: "min",
  unit: "s",
  run_tags: {},
  info: {},
  change_annotations: {},
  zscorestats: null,
});

const history = [
  historySample("r0", "abc0000", "2024-01-06T12:00:00Z", 2),
  historySample("r1", "abc1234def", "2024-01-07T12:00:00Z", 1.5),
];

beforeEach(() => {
  GET.mockReset();
  PUT.mockReset();
  DELETE.mockReset();
});

function mockPage(
  result: ResultDetail = detail,
  capabilities = readOnlyCapabilities,
  samples: HistorySample[] = history,
) {
  GET.mockImplementation((path: string) => {
    if (path === "/api/benchmark-results/{id}") {
      return Promise.resolve({ data: result });
    }
    if (path === "/api/auth/capabilities") {
      return Promise.resolve({ data: capabilities });
    }
    if (path === "/api/history/{benchmark_result_id}") {
      return Promise.resolve({ data: { history_fingerprint: "fp1", samples } });
    }
    return Promise.resolve({ error: { detail: `unexpected GET ${path}` } });
  });
}

describe("ResultPage", () => {
  it("presents the selected result inside its series trend before record details", async () => {
    mockPage();
    render(ResultPage, { props: { resultId: "r1" } });
    await waitFor(() => screen.getByRole("heading", { name: "demo-benchmark" }));
    expect(screen.getByRole("main")).toHaveClass("page");
    const measurement = screen.getByRole("region", { name: /result measurement/i });
    expect(measurement).toHaveTextContent("1.5 s");
    expect(measurement).toHaveTextContent("Lower is better");
    expect(screen.getByRole("region", { name: /result facts/i })).toBeInTheDocument();
    expect(screen.getByText("scale=sf10")).toBeInTheDocument();
    expect(screen.getByText("sha").nextElementSibling).toHaveTextContent("abc1234d");
    expect(screen.getByText("sha").nextElementSibling).toHaveAttribute("title", "abc1234def");
    expect(screen.getByText("run1")).toBeInTheDocument();
    const trend = screen.getByRole("region", { name: /result in series trend/i });
    expect(trend.compareDocumentPosition(screen.getByRole("region", { name: /result measurement/i })))
      .toBe(Node.DOCUMENT_POSITION_FOLLOWING);
    expect(trend).toHaveTextContent(/25\.0% better than previous/i);
    expect(document.querySelector(".chart-stub")).toHaveAttribute("data-current-index", "1");
    expect(screen.getByRole("link", { name: /explore full series/i })).toHaveAttribute(
      "href",
      "/benchmarks/b1",
    );
    expect(screen.getByRole("link", { name: /export history json/i })).toHaveAttribute("href", "/api/history/r1");
    expect(screen.getByRole("heading", { name: "Machine" })).toBeInTheDocument();
    expect(screen.getAllByText("m5").length).toBeGreaterThan(0);
    expect(screen.getByRole("group", { name: /technical details/i })).not.toHaveAttribute("open");
    expect(screen.getByText("Environment details").closest("details")).not.toHaveAttribute("open");
    expect(screen.getByText("Identifiers").closest("details")).not.toHaveAttribute("open");
    expect(screen.getByText("raw data").nextElementSibling).toHaveTextContent("3 values");
    expect(screen.getByText("raw times").nextElementSibling).toHaveTextContent("3 values");
    expect(screen.getAllByText(/"suite": "arrow"/).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/"owner": "perf"/).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/"validator": "pandas.testing"/).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/"note": "checked"/).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/"data": \[/).length).toBeGreaterThan(0);
  });

  it("does not show write actions to signed-out viewers", async () => {
    mockPage();

    render(ResultPage, { props: { resultId: "r1" } });
    await waitFor(() => screen.getByRole("heading", { name: "demo-benchmark" }));

    expect(screen.queryByRole("button", { name: /mark distribution change/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /delete result/i })).toBeNull();
  });

  it("suppresses comparisons and charts when a history mixes units", async () => {
    const mixedHistory = [
      historySample("r0", "abc0000", "2024-01-06T12:00:00Z", 2),
      { ...historySample("r1", "abc1234def", "2024-01-07T12:00:00Z", 1.5), unit: "ms" },
    ];
    mockPage(detail, readOnlyCapabilities, mixedHistory);

    render(ResultPage, { props: { resultId: "r1" } });

    const warning = await screen.findByRole("alert");
    expect(warning).toHaveTextContent(/mixes units \(s, ms\)/i);
    expect(warning).toHaveTextContent(/cannot be compared or plotted/i);
    expect(screen.queryByText(/better than previous/i)).toBeNull();
    expect(document.querySelector(".chart-stub")).toBeNull();
  });

  it("shows a terminal state when the result has no comparable history", async () => {
    mockPage(detail, readOnlyCapabilities, []);

    render(ResultPage, { props: { resultId: "r1" } });

    await waitFor(() => screen.getByRole("heading", { name: "demo-benchmark" }));
    expect(screen.getByText(/no comparable default-branch history/i)).toBeInTheDocument();
    expect(screen.queryByText(/loading series history/i)).toBeNull();
    expect(screen.queryByRole("link", { name: /explore full series/i })).toBeNull();
  });

  it("shows write actions when auth-disabled dev mode allows result writes", async () => {
    mockPage(detail, authDisabledCapabilities);

    render(ResultPage, { props: { resultId: "r1" } });
    await waitFor(() => screen.getByRole("heading", { name: "demo-benchmark" }));

    expect(screen.getByRole("button", { name: /mark distribution change/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /delete result/i })).toBeInTheDocument();
  });

  it("marks and unmarks a result as a distribution change", async () => {
    let historyReads = 0;
    GET.mockImplementation((path: string) => {
      if (path === "/api/benchmark-results/{id}") {
        return Promise.resolve({ data: { ...detail, change_annotations: {} } });
      }
      if (path === "/api/auth/capabilities") {
        return Promise.resolve({ data: signedInCapabilities });
      }
      if (path === "/api/history/{benchmark_result_id}") {
        historyReads++;
        const samples = history.map((sample) => sample.benchmark_result_id === "r1"
          ? {
              ...sample,
              change_annotations: historyReads === 2
                ? { begins_distribution_change: true }
                : {},
            }
          : sample);
        return Promise.resolve({ data: { history_fingerprint: "fp1", samples } });
      }
      return Promise.resolve({ error: { detail: `unexpected GET ${path}` } });
    });
    PUT.mockResolvedValueOnce({ data: { ...detail, change_annotations: { begins_distribution_change: true } } });
    PUT.mockResolvedValueOnce({ data: { ...detail, change_annotations: {} } });

    render(ResultPage, { props: { resultId: "r1" } });
    await waitFor(() => screen.getByRole("heading", { name: "demo-benchmark" }));

    await fireEvent.click(screen.getByRole("button", { name: /mark distribution change/i }));
    await waitFor(() =>
      expect(PUT).toHaveBeenCalledWith("/api/benchmark-results/{id}", {
        params: { path: { id: "r1" } },
        body: { change_annotations: { begins_distribution_change: true } },
      }),
    );
    expect(screen.getByText(/annotation updated/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /unmark distribution change/i })).toBeInTheDocument();
    expect(screen.getByText("step", { selector: ".flag" })).toBeInTheDocument();

    await fireEvent.click(screen.getByRole("button", { name: /unmark distribution change/i }));
    await waitFor(() =>
      expect(PUT).toHaveBeenLastCalledWith("/api/benchmark-results/{id}", {
        params: { path: { id: "r1" } },
        body: { change_annotations: { begins_distribution_change: null } },
      }),
    );
    expect(screen.getByRole("button", { name: /mark distribution change/i })).toBeInTheDocument();
    await waitFor(() => expect(document.querySelector(".flag")).toBeNull());
    expect(historyReads).toBe(3);
  });

  it("deletes a result after confirmation", async () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    mockPage(detail, signedInCapabilities);
    DELETE.mockResolvedValueOnce({ response: { status: 204 } });

    render(ResultPage, { props: { resultId: "r1" } });
    await waitFor(() => screen.getByRole("heading", { name: "demo-benchmark" }));

    await fireEvent.click(screen.getByRole("button", { name: /delete result/i }));
    expect(confirm).toHaveBeenCalledWith("Delete result r1?");
    await waitFor(() =>
      expect(DELETE).toHaveBeenCalledWith("/api/benchmark-results/{id}", {
        params: { path: { id: "r1" } },
      }),
    );
    expect(screen.getByRole("heading", { name: /result deleted/i })).toBeInTheDocument();
    confirm.mockRestore();
  });

  it("shows the error payload for an errored result", async () => {
    mockPage({ ...detail, single_value_summary: null, error: { stack: "trace" } });
    render(ResultPage, { props: { resultId: "r1" } });
    await waitFor(() => screen.getByRole("heading", { name: "demo-benchmark" }));
    expect(screen.getByRole("region", { name: /result measurement/i })).toHaveTextContent("—");
    expect(screen.getAllByText(/"stack"/).length).toBeGreaterThan(0);
  });

  it("shows the error state when loading fails", async () => {
    GET.mockImplementation((path: string) => {
      if (path === "/api/auth/capabilities") {
        return Promise.resolve({ data: readOnlyCapabilities });
      }
      return Promise.resolve({ error: { detail: "boom" } });
    });
    render(ResultPage, { props: { resultId: "rX" } });
    await waitFor(() => expect(screen.getByText(/failed to load/i)).toBeInTheDocument());
  });
});
