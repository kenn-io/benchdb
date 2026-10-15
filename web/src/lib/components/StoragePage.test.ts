import { getBenchDB, type StorageStatus } from "../api/benchdb";
import type { AxiosInstance } from "axios";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import StoragePage from "./StoragePage.svelte";

const GET = vi.fn();
vi.mock("../api/client", () => ({
  createBenchDBClient: () => getBenchDB({ get: GET } as unknown as AxiosInstance),
}));

const sample: StorageStatus = {
  sampled_at: "2026-10-14T12:00:00Z",
  postgres_bytes: 240_000_000,
  pending_rows_estimate: 2_500,
  archived_rows: 150_000,
  parquet_bytes: 3_000_000_000,
  parquet_files: 12,
  catalog_bytes: 14_000_000,
  catalog_files: 3,
  pending_mutations: 4,
  stale_catalogs: 2,
  volume_total_bytes: 10_000_000_000,
  volume_available_bytes: 2_000_000_000,
  maintenance: {
    enabled: true,
    state: "scheduled",
    interval_seconds: 3600,
    next_run_at: "2026-10-14T13:00:00Z",
    last_started_at: "2026-10-14T12:00:00Z",
    last_finished_at: "2026-10-14T12:03:00Z",
    last_success_at: "2026-10-14T12:03:00Z",
    phase: "idle",
    error: "",
    exported_rows: 12_000,
    compacted_files: 8,
    refreshed_catalogs: 2,
    removed_files: 5,
  },
};

function respond(data: StorageStatus) {
  GET.mockImplementation((path: string) => {
    if (path !== "/api/storage") throw new Error(`unexpected GET ${path}`);
    return Promise.resolve({ status: 200, data });
  });
}

beforeEach(() => {
  GET.mockReset();
  respond(sample);
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("StoragePage", () => {
  it("shows storage amounts, estimated pending results, and volume capacity", async () => {
    render(StoragePage);
    expect(screen.getByRole("status")).toHaveTextContent("Loading storage");

    const postgres = await screen.findByRole("region", { name: "PostgreSQL" });
    expect(within(postgres).getByText("240 MB")).toBeInTheDocument();
    expect(within(postgres).getByText("Pending results (estimate)")).toBeInTheDocument();
    expect(within(postgres).getByText("2,500")).toBeInTheDocument();
    const parquet = screen.getByRole("region", { name: "Parquet archive" });
    expect(within(parquet).getByText("3 GB")).toBeInTheDocument();
    expect(within(parquet).getByText("150,000")).toBeInTheDocument();
    expect(within(parquet).getByText("12")).toBeInTheDocument();
    const catalogs = screen.getByRole("region", { name: "Catalogs" });
    expect(within(catalogs).getByText("14 MB")).toBeInTheDocument();
    expect(within(catalogs).getByText("3")).toBeInTheDocument();
    const volume = screen.getByRole("region", { name: "Archive volume" });
    expect(within(volume).getByText("2 GB")).toBeInTheDocument();
    expect(within(volume).getByText("10 GB")).toBeInTheDocument();
    expect(within(volume).getByRole("meter")).toHaveAttribute("value", "8000000000");
    expect(within(volume).getByRole("meter")).toHaveAttribute("max", "10000000000");
  });

  it("shows the schedule, last success, pending work, and latest cycle counts", async () => {
    render(StoragePage);

    const maintenance = await screen.findByRole("region", { name: "Maintenance" });
    expect(within(maintenance).getByText("Scheduled")).toBeInTheDocument();
    expect(within(maintenance).getByText("Every 1 hour")).toBeInTheDocument();
    const lastSuccess = within(maintenance).getByText("Last success").parentElement!;
    expect(lastSuccess.querySelector("time")).toHaveAttribute("datetime", "2026-10-14T12:03:00Z");
    const nextRun = within(maintenance).getByText("Next run").parentElement!;
    expect(nextRun.querySelector("time")).toHaveAttribute("datetime", "2026-10-14T13:00:00Z");
    expect(within(maintenance).getByText("Pending edits").parentElement).toHaveTextContent("4");
    expect(within(maintenance).getByText("Files awaiting catalog refresh").parentElement).toHaveTextContent("2");
    const cycle = within(maintenance).getByRole("region", { name: "Latest cycle" });
    expect(within(cycle).getByText("Results exported").parentElement).toHaveTextContent("12,000");
    expect(within(cycle).getByText("Files compacted").parentElement).toHaveTextContent("8");
    expect(within(cycle).getByText("Files with refreshed catalogs").parentElement).toHaveTextContent("2");
    expect(within(cycle).getByText("Old files removed").parentElement).toHaveTextContent("5");
  });

  it("shows the running phase and current cycle", async () => {
    const maintenanceState = { ...sample.maintenance };
    delete maintenanceState.next_run_at;
    respond({ ...sample, maintenance: { ...maintenanceState, state: "running", phase: "compact" } });
    render(StoragePage);

    const maintenance = await screen.findByRole("region", { name: "Maintenance" });
    expect(within(maintenance).getByText("Running")).toBeInTheDocument();
    expect(within(maintenance).getByText("Compacting files")).toBeInTheDocument();
    expect(within(maintenance).getByText("Next run").parentElement).toHaveTextContent("1 hour after this cycle");
    expect(within(maintenance).getByRole("region", { name: "Current cycle" })).toBeInTheDocument();
  });

  it("distinguishes disabled maintenance from a cycle that has never run", async () => {
    const maintenanceState = { ...sample.maintenance };
    delete maintenanceState.next_run_at;
    delete maintenanceState.last_started_at;
    delete maintenanceState.last_finished_at;
    delete maintenanceState.last_success_at;
    respond({ ...sample, maintenance: { ...maintenanceState, enabled: false, state: "disabled" } });
    render(StoragePage);

    const maintenance = await screen.findByRole("region", { name: "Maintenance" });
    expect(within(maintenance).getByText("Disabled")).toBeInTheDocument();
    expect(within(maintenance).getByText("Next run").parentElement).toHaveTextContent("Not scheduled");
    expect(within(maintenance).getByText("Last success").parentElement).toHaveTextContent("Not yet");
    expect(within(maintenance).queryByRole("region", { name: "Latest cycle" })).toBeNull();
  });

  it.each(["failed", "interrupted"] as const)("shows %s maintenance with its stage and last success", async (state) => {
    respond({ ...sample, maintenance: {
      ...sample.maintenance, state, phase: "export", error: "Export failed. See server logs.",
    } });
    render(StoragePage);

    const maintenance = await screen.findByRole("region", { name: "Maintenance" });
    expect(within(maintenance).getByText(state === "failed" ? "Failed" : "Interrupted")).toBeInTheDocument();
    expect(within(maintenance).getByText("Exporting results")).toBeInTheDocument();
    expect(within(maintenance).getByText("Export failed. See server logs.")).toBeInTheDocument();
    expect(within(maintenance).getByText("Last success").parentElement!.querySelector("time"))
      .toHaveAttribute("datetime", "2026-10-14T12:03:00Z");
  });

  it.each(["http", "network"])("offers retry after a %s error without exposing server details", async (failure) => {
    if (failure === "http") GET.mockResolvedValueOnce({ status: 500, data: { detail: "open /private/storage: denied" } });
    else GET.mockRejectedValueOnce(new Error("open /private/storage: denied"));
    render(StoragePage);

    expect(await screen.findByRole("alert")).toHaveTextContent("Could not load storage.");
    expect(screen.queryByText(/private\/storage/)).toBeNull();
    await fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(await screen.findByRole("region", { name: "PostgreSQL" })).toHaveTextContent("240 MB");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("retains the previous sample after a failed refresh and replaces it on retry", async () => {
    render(StoragePage);
    await screen.findByRole("region", { name: "PostgreSQL" });

    GET.mockRejectedValueOnce(new Error("network offline"));
    await fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not refresh storage. Showing the last available sample.");
    expect(screen.getByRole("region", { name: "PostgreSQL" })).toHaveTextContent("240 MB");

    respond({ ...sample, postgres_bytes: 360_000_000 });
    await fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(screen.getByRole("region", { name: "PostgreSQL" })).toHaveTextContent("360 MB"));
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("refreshes visible pages without overlapping requests and aborts on unmount", async () => {
    vi.useFakeTimers();
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    const { unmount } = render(StoragePage);
    await act(() => Promise.resolve());
    expect(screen.getByRole("region", { name: "PostgreSQL" })).toHaveTextContent("240 MB");

    respond({ ...sample, postgres_bytes: 480_000_000 });
    await act(() => vi.advanceTimersByTimeAsync(30_000));
    expect(screen.getByRole("region", { name: "PostgreSQL" })).toHaveTextContent("480 MB");

    vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    await act(() => vi.advanceTimersByTimeAsync(30_000));
    expect(GET).toHaveBeenCalledTimes(2);

    vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    let signal: AbortSignal | undefined;
    GET.mockImplementation((path: string, options: { signal: AbortSignal }) => {
      expect(path).toBe("/api/storage");
      signal = options.signal;
      return new Promise(() => {});
    });
    await act(() => vi.advanceTimersByTimeAsync(30_000));
    expect(screen.getByRole("button", { name: "Refresh" })).toBeDisabled();
    await act(() => vi.advanceTimersByTimeAsync(60_000));
    expect(GET).toHaveBeenCalledTimes(3);

    unmount();
    expect(signal?.aborted).toBe(true);
    await act(() => vi.advanceTimersByTimeAsync(30_000));
    expect(GET).toHaveBeenCalledTimes(3);
  });
});
