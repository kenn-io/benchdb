import { render, screen } from "@testing-library/svelte";
import { afterEach, describe, expect, it } from "vitest";

import SubmitResultsHint from "./SubmitResultsHint.svelte";

afterEach(() => document.querySelector("base")?.remove());

describe("SubmitResultsHint", () => {
  it("quotes a base path the shell would otherwise split", () => {
    const base = document.createElement("base");
    base.href = "/tools/bench&dev/";
    document.head.append(base);

    render(SubmitResultsHint);

    expect(screen.getByText(
      `benchdb results submit "bench-results/*.json" --server '${window.location.origin}/tools/bench&dev'`,
    )).toBeInTheDocument();
  });
});
