import { describe, expect, it } from "vitest";

import { summarizeEnvironment } from "./environment";

describe("summarizeEnvironment", () => {
  it("turns a production-shaped context into compact human labels", () => {
    const summary = summarizeEnvironment({
      environment_epoch_id: "epoch-0123456789abcdef01234567",
      os_release: 'ID=ubuntu\nPRETTY_NAME="Ubuntu 26.04 LTS"\nVERSION_ID="26.04"',
      kernel_release: "7.0.0-30-generic",
      go_version: "go1.27.0",
      governor: "powersave",
      smt: "0",
      turbo: "0",
      go_build_settings: "GOTOOLCHAIN=local\nGOPROXY=off",
    });

    expect(summary).toEqual({
      epoch: "epoch-01234567…4567",
      highlights: [
        "Ubuntu 26.04 LTS",
        "kernel 7.0.0-30-generic",
        "Go 1.27.0",
        "governor powersave",
        "SMT off",
        "turbo on",
      ],
    });
  });
});
