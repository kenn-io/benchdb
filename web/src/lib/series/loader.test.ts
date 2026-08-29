import { describe, expect, it, vi } from "vitest";

import type { createBenchDBClient } from "../api/client";
import type { components } from "../api/schema";
import { loadTrend } from "./loader";

type Client = ReturnType<typeof createBenchDBClient>;
type HistorySample = components["schemas"]["HistorySample"];
type BenchmarkHistory = components["schemas"]["BenchmarkHistory"];

const sample = (id: string, ts: string): HistorySample => ({
  benchmark_result_id: id,
  commit_hash: `sha-${id}`,
  commit_message: "msg",
  commit_repository: "https://github.com/benchdb/demo",
  commit_timestamp: ts,
  data: null,
  hardware_hash: "hw1",
  mean: 1.1,
  result_timestamp: ts,
  single_value_summary: 1.1,
  single_value_summary_type: "min",
  unit: "s",
  run_tags: {},
  info: {},
  change_annotations: {},
  zscorestats: null,
});

function history(over: Partial<BenchmarkHistory> = {}): BenchmarkHistory {
  return {
    benchmark_id: "benchmark-1",
    name: "demo-benchmark",
    tags: { name: "demo-benchmark", scale: "sf10" },
    repository: "https://github.com/benchdb/demo",
    unit: "s",
    less_is_better: true,
    tracks: [
      {
        machine_name: "m5",
        segments: [
          {
            history_fingerprint: "fp1",
            context: { compiler: "gcc" },
            hardware: { id: "h1", type: "machine", name: "m5", hash: "hw1" },
            samples: [sample("r2", "2024-01-08T12:00:00Z"), sample("r1", "2024-01-07T12:00:00Z")],
          },
        ],
      },
    ],
    ...over,
  };
}

describe("loadTrend", () => {
  it("loads all machine tracks for a stable benchmark id", async () => {
    const GET = vi.fn(async (url: string) => {
      if (url === "/api/benchmarks/{benchmark_id}") return { data: history() };
      throw new Error(`unexpected url ${url}`);
    });
    const vm = await loadTrend({ GET } as unknown as Client, {
      kind: "benchmark",
      benchmarkId: "benchmark-1",
    });
    expect(vm.identity).toMatchObject({
      benchmarkId: "benchmark-1",
      benchmarkName: "demo-benchmark",
      caseTags: { scale: "sf10" },
      repositoryLabel: "benchdb/demo",
      unit: "s",
      lessIsBetter: true,
    });
    expect(vm.tracks).toHaveLength(1);
    expect(vm.tracks[0]?.machineName).toBe("m5");
    expect(vm.tracks[0]?.points.map((point) => point.resultId)).toEqual(["r1", "r2"]);
  });

  it("resolves a result to its benchmark before loading fleet history", async () => {
    const GET = vi.fn(async (url: string) => {
      if (url === "/api/benchmark-results/{id}") {
        return { data: { benchmark_id: "benchmark-1", error: null, commit: { sha: "abc123" } } };
      }
      if (url === "/api/benchmarks/{benchmark_id}") return { data: history() };
      throw new Error(`unexpected url ${url}`);
    });
    const vm = await loadTrend({ GET } as unknown as Client, { kind: "result", resultId: "r1" });
    expect(GET).toHaveBeenCalledTimes(2);
    expect(vm.identity.benchmarkId).toBe("benchmark-1");
  });

  it("resolves an errored result to the benchmark's available history", async () => {
    const GET = vi.fn(async (url: string) => {
      if (url === "/api/benchmark-results/{id}") {
        return {
          data: {
            benchmark_id: "benchmark-1",
            error: { message: "failed" },
            commit: { sha: "abc123" },
          },
        };
      }
      if (url === "/api/benchmarks/{benchmark_id}") return { data: history() };
      throw new Error(`unexpected url ${url}`);
    });

    const vm = await loadTrend(
      { GET } as unknown as Client,
      { kind: "result", resultId: "errored-result" },
    );

    expect(GET).toHaveBeenCalledTimes(2);
    expect(vm.identity.benchmarkId).toBe("benchmark-1");
  });

  it("rejects a commitless result before requesting unavailable history", async () => {
    const result = { benchmark_id: "benchmark-1", error: null, commit: null };
    const GET = vi.fn(async (url: string) => {
      if (url === "/api/benchmark-results/{id}") return { data: result };
      throw new Error(`unexpected url ${url}`);
    });

    await expect(
      loadTrend({ GET } as unknown as Client, { kind: "result", resultId: "r1" }),
    ).rejects.toThrow(/no comparable default-branch history/i);
    expect(GET).toHaveBeenCalledTimes(1);
  });

  it("reports an empty logical benchmark as unavailable history", async () => {
    const GET = vi.fn(async (url: string) => {
      if (url === "/api/benchmark-results/{id}") {
        return { data: { benchmark_id: "benchmark-1", error: null, commit: { sha: "abc123" } } };
      }
      if (url === "/api/benchmarks/{benchmark_id}") {
        return { error: { detail: "not found" }, response: { status: 404 } };
      }
      throw new Error(`unexpected url ${url}`);
    });

    await expect(
      loadTrend({ GET } as unknown as Client, { kind: "result", resultId: "r1" }),
    ).rejects.toThrow(/no comparable default-branch history/i);
  });

  it("keeps context epochs as segments under one machine", async () => {
    const h = history();
    h.tracks![0]!.segments!.push({
      history_fingerprint: "fp2",
      context: { compiler: "clang" },
      hardware: { id: "h2", type: "machine", name: "m5", hash: "hw2" },
      samples: [sample("r3", "2024-01-09T12:00:00Z")],
    });
    const GET = vi.fn(async () => ({ data: h }));
    const vm = await loadTrend({ GET } as unknown as Client, { kind: "benchmark", benchmarkId: "benchmark-1" });
    expect(vm.tracks[0]?.segments.map((segment) => segment.fingerprint)).toEqual(["fp1", "fp2"]);
    expect(vm.tracks[0]?.points.map((point) => point.resultId)).toEqual(["r1", "r2", "r3"]);
  });

  it("treats null tracks and samples as empty", async () => {
    const GET = vi.fn(async () => ({ data: history({ tracks: null }) }));
    const vm = await loadTrend({ GET } as unknown as Client, { kind: "benchmark", benchmarkId: "benchmark-1" });
    expect(vm.tracks).toEqual([]);
  });

  it("throws when a benchmark cannot be loaded", async () => {
    const GET = vi.fn(async () => ({ error: { detail: "not found" } }));
    await expect(
      loadTrend({ GET } as unknown as Client, { kind: "benchmark", benchmarkId: "missing" }),
    ).rejects.toThrow("missing");
  });
});
