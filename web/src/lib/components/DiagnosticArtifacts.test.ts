import { fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { afterEach, describe, expect, it, vi } from "vitest";
import DiagnosticArtifacts from "./DiagnosticArtifacts.svelte";

const DELETE = vi.fn();
vi.mock("../api/client", () => ({ createBenchDBClient: () => ({ DELETE }) }));
afterEach(() => { vi.restoreAllMocks(); DELETE.mockReset(); });

describe("DiagnosticArtifacts", () => {
  it("explains when a result has no diagnostic evidence", () => {
    render(DiagnosticArtifacts, { props: { result: { id: "r1", artifacts: [], diagnostics: null } } });
    expect(screen.getByText("No diagnostic files were attached to this result.")).toBeInTheDocument();
  });

  it("shows the capture failure while retaining downloads from the partial capture", () => {
    render(DiagnosticArtifacts, { props: { result: {
      id: "run/1",
      diagnostics: { state: "failed", failure_class: "profile-invocation" },
      artifacts: [{ id: "artifact-1", name: "fresh-sync-diagnostics.json", kind: "diagnostics", media_type: "application/json", size_bytes: 200, sha256: "digest" }],
    } } });
    expect(screen.getByText(/diagnostic capture failed/i)).toBeInTheDocument();
    expect(screen.getByText("Reason: profile-invocation")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "fresh-sync-diagnostics.json" })).toHaveAttribute("href", "/api/benchmark-results/run%2F1/artifacts/artifact-1");
  });
  it("deletes only the selected attachment and reports failures without hiding it", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    DELETE.mockResolvedValueOnce({ error: { detail: "Storage is unavailable. Try again." } });
    render(DiagnosticArtifacts, { props: { canWrite: true, result: {
      id: "r1", diagnostics: null,
      artifacts: [{ id: "artifact-1", name: "CPU Profile.pprof", kind: "cpu-profile", media_type: "application/vnd.google.pprof", size_bytes: 1073741824, sha256: "digest" }],
    } } });
    await fireEvent.click(screen.getByRole("button", { name: "Delete CPU Profile.pprof" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Storage is unavailable. Try again.");
    expect(screen.getByRole("link", { name: "CPU Profile.pprof" })).toBeInTheDocument();
    DELETE.mockResolvedValueOnce({ response: { status: 204 } });
    await fireEvent.click(screen.getByRole("button", { name: "Delete CPU Profile.pprof" }));
    await waitFor(() => expect(screen.queryByRole("link", { name: "CPU Profile.pprof" })).not.toBeInTheDocument());
    expect(DELETE).toHaveBeenLastCalledWith("/api/benchmark-results/{id}/artifacts/{artifact_id}", {
      params: { path: { id: "r1", artifact_id: "artifact-1" } },
    });
    expect(screen.getByRole("status")).toHaveTextContent("Deleted CPU Profile.pprof.");
  });

});
