import { fireEvent, render, screen } from "@testing-library/svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import MeasurementValue from "./MeasurementValue.svelte";

afterEach(() => {
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
  Reflect.deleteProperty(document, "execCommand");
});

describe("MeasurementValue", () => {
  it("shows a readable byte value and copies the unformatted number", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    });
    render(MeasurementValue, { props: { value: 12_263_215_104, unit: "B" } });

    const value = screen.getByRole("button", { name: /12\.263 GB/i });
    expect(value).toHaveAttribute("title", "12,263,215,104 B — click to copy the exact number");
    await fireEvent.click(value);
    expect(writeText).toHaveBeenCalledWith("12263215104");
    expect(screen.getByRole("status")).toHaveTextContent("Copied");
  });

  it("copies from the direct HTTP dashboard when the Clipboard API is unavailable", async () => {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
    const execCommand = vi.fn().mockReturnValue(true);
    Object.defineProperty(document, "execCommand", { configurable: true, value: execCommand });
    render(MeasurementValue, { props: { value: 12_263_215_104, unit: "B" } });

    await fireEvent.click(screen.getByRole("button", { name: /12\.263 GB/i }));
    expect(execCommand).toHaveBeenCalledWith("copy");
    expect(screen.getByRole("status")).toHaveTextContent("Copied");
  });
});
