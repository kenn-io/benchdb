import { render, screen } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";
import DiagnosticArtifacts from "./DiagnosticArtifacts.svelte";

describe("DiagnosticArtifacts", () => {
  it("explains when a result has no diagnostic evidence", () => {
    render(DiagnosticArtifacts, { props: { result: { id: "r1", artifacts: [], diagnostics: null } } });
    expect(screen.getByText("No diagnostic files were attached to this result.")).toBeInTheDocument();
  });

  it("shows the capture failure while retaining downloads from the partial capture", () => {
    render(DiagnosticArtifacts, { props: { result: {
      id: "run/1",
      diagnostics: { state: "failed", failure_class: "profile-invocation" },
      artifacts: [{ name: "fresh-sync-diagnostics.json", kind: "diagnostics", media_type: "application/json", size_bytes: 200, sha256: "digest" }],
    } } });
    expect(screen.getByText(/diagnostic capture failed/i)).toBeInTheDocument();
    expect(screen.getByText("Reason: profile-invocation")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "fresh-sync-diagnostics.json" })).toHaveAttribute("href", "/api/benchmark-results/run%2F1/artifacts/fresh-sync-diagnostics.json");
  });
});
