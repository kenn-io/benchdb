import { getBenchDB } from "./lib/api/benchdb";
import type { AxiosInstance } from "axios";
import { fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { beforeEach, describe, expect, it, vi } from "vitest";

import App from "./App.svelte";
import { navigate } from "./lib/router";

// Pages stay in their loading state; these tests cover the shell around them.
const GET = vi.fn();
vi.mock("./lib/api/client", () => ({
  createBenchDBClient: () => getBenchDB({ get: GET } as unknown as AxiosInstance),
}));

beforeEach(() => {
  localStorage.clear();
  GET.mockReset();
  GET.mockReturnValue(new Promise(() => {}));
  window.history.replaceState(null, "", "/no-such-page");
});

describe("App", () => {
  it("opens storage from the sidebar and names the page", async () => {
    render(App);

    await fireEvent.click(screen.getByRole("link", { name: "Storage" }));

    await waitFor(() => expect(screen.getByRole("heading", { name: "Storage" })).toBeInTheDocument());
    expect(window.location.pathname).toBe("/storage");
    expect(document.title).toBe("Storage · BenchDB");
    expect(screen.getByRole("link", { name: "Storage" })).toHaveAttribute("aria-current", "page");
  });

  it("names the document after the current page", async () => {
    render(App);
    expect(document.title).toBe("Not found · BenchDB");

    navigate("/runs/run-7");
    await waitFor(() => expect(document.title).toBe("Run run-7 · BenchDB"));

    navigate("/series");
    await waitFor(() => expect(document.title).toBe("Benchmarks · BenchDB"));
  });

  it("offers a skip link that moves focus past the sidebar", async () => {
    render(App);
    const skip = screen.getByRole("link", { name: "Skip to content" });
    expect(document.body.querySelector("a")).toBe(skip);

    await fireEvent.click(skip);
    expect(document.activeElement).toHaveAttribute("id", "main-content");
    expect(document.activeElement).toContainElement(screen.getByRole("heading", { name: "Not found" }));
  });
});
