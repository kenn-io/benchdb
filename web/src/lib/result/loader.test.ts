import { describe, expect, it, vi } from "vitest";

import type { createBenchDBClient } from "../api/client";
import { loadResult, loadResultHistory } from "./loader";

type Client = ReturnType<typeof createBenchDBClient>;

const detail = {
  id: "r1",
  benchmark_id: "benchmark-1",
  batch_id: "b1",
  run_id: "run1",
  run_reason: "commit",
  run_tags: { ci: "true" },
  tags: { name: "demo-benchmark", scale: "sf10" },
  context: { compiler: "gcc" },
  info: {},
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
  single_value_summary: 1.23456,
  single_value_summary_type: "min",
  iterations: 3,
  data: [1.2, 1.3, 1.25],
  times: null,
  time_unit: null,
  error: null,
  stats: { min: 1.2, max: 1.3, mean: 1.25, median: 1.25, q1: null, q3: null, stdev: 0.05, iqr: null },
  history_fingerprint: "fp1",
  timestamp: "2024-01-07T13:00:00Z",
};

function fakeClient(data: unknown, error = false): Client {
  return { GET: vi.fn(async () => (error ? { error: { detail: "boom" } } : { data })) } as unknown as Client;
}

describe("loadResult", () => {
  it("maps the detail payload onto the view-model", async () => {
    const vm = await loadResult(fakeClient(detail), "r1");
    expect(vm).toMatchObject({
      id: "r1",
      name: "demo-benchmark",
      paramsText: "scale=sf10",
      context: { compiler: "gcc" },
      svsText: "1.235 s",
      svsType: "min",
      iterations: 3,
      hardwareName: "m5",
      hardwareType: "machine",
      commitSha: "abc1234def",
      commitMessage: "tune",
      repository: "https://github.com/benchdb/demo",
      runId: "run1",
      runReason: "commit",
      batchId: "b1",
      fingerprint: "fp1",
      benchmarkId: "benchmark-1",
      error: null,
    });
    expect(vm.aggregates).toContainEqual({ label: "mean", value: "1.25 s" });
    expect(vm.aggregates.some((a) => a.label === "q1")).toBe(false);
  });

  it("derives compact display labels for exact identifiers", async () => {
    const vm = await loadResult(fakeClient({
      ...detail,
      id: "06a220d0d94471c480001414453ee7fc",
      batch_id: "66f23037065241d6ac22aaeaea96d29b-1p",
      run_id: "66f23037065241d6ac22aaeaea96d29b",
      run_tags: { name: "commit:2315161817ad5dcb94891567e7ac48a35921e05a" },
      hardware: {
        ...detail.hardware,
        hash: "0123456789abcdef0123456789abcdef",
      },
      commit_repo_url: "https://github.com/apache/arrow",
      commit: {
        ...detail.commit,
        repository: "https://github.com/apache/arrow",
        sha: "2315161817ad5dcb94891567e7ac48a35921e05a",
      },
      history_fingerprint: "fff41571debd35f721110e6a7d99440a",
    }), "06a220d0d94471c480001414453ee7fc");

    expect(vm.displayResultId).toBe("06a220d0d944…453ee7fc");
    expect(vm.displayRunId).toBe("66f230370652…ea96d29b");
    expect(vm.displayBatchId).toBe("66f230370652…6d29b-1p");
    expect(vm.displayHardwareHash).toBe("0123456789ab…89abcdef");
    expect(vm.displayFingerprint).toBe("fff41571debd…7d99440a");
    expect(vm.shortCommit).toBe("23151618");
    expect(vm.repositoryLabel).toBe("apache/arrow");
    expect(vm.runTagsText).toBe("name=commit:2315161817ad…5921e05a");
  });

  it("renders an errored result with a dash SVS and the error payload", async () => {
    const vm = await loadResult(
      fakeClient({ ...detail, single_value_summary: null, error: { stack: "trace" } }),
      "r1",
    );
    expect(vm.svsText).toBe("—");
    expect(vm.error).toEqual({ stack: "trace" });
  });

  it("handles a result with no commit association", async () => {
    const vm = await loadResult(fakeClient({ ...detail, commit: null }), "r1");
    expect(vm.commitSha).toBeNull();
    expect(vm.commitMessage).toBeNull();
    expect(vm.commitDateText).toBeNull();
    expect(vm.repository).toBe("https://github.com/benchdb/demo");
  });

  it("throws when the endpoint errors", async () => {
    await expect(loadResult(fakeClient(null, true), "rX")).rejects.toThrow("rX");
  });
});

describe("loadResultHistory", () => {
  it("orders the result's series for an in-context trend", async () => {
    const samples = [
      {
        benchmark_result_id: "r2",
        commit_hash: "def5678",
        commit_message: "after",
        commit_repository: "https://github.com/benchdb/demo",
        commit_timestamp: "2024-01-08T12:00:00Z",
        data: null,
        hardware_hash: "hw1",
        mean: 1.1,
        result_timestamp: "2024-01-08T13:00:00Z",
        single_value_summary: 1.1,
        single_value_summary_type: "min",
        unit: "s",
        run_tags: {},
        info: {},
        change_annotations: {},
        zscorestats: null,
      },
      {
        benchmark_result_id: "r1",
        commit_hash: "abc1234",
        commit_message: "before",
        commit_repository: "https://github.com/benchdb/demo",
        commit_timestamp: "2024-01-07T12:00:00Z",
        data: null,
        hardware_hash: "hw1",
        mean: 1.2,
        result_timestamp: "2024-01-07T13:00:00Z",
        single_value_summary: 1.2,
        single_value_summary_type: "min",
        unit: "s",
        run_tags: {},
        info: {},
        change_annotations: {},
        zscorestats: null,
      },
    ];
    const client = {
      GET: vi.fn(async () => ({ data: { history_fingerprint: "fp1", samples } })),
    } as unknown as Client;

    const points = await loadResultHistory(client, "r1");

    expect(points.map((point) => point.resultId)).toEqual(["r1", "r2"]);
    expect(client.GET).toHaveBeenCalledWith("/api/history/{benchmark_result_id}", {
      params: { path: { benchmark_result_id: "r1" } },
    });
  });
});
