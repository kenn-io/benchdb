import { describe, expect, it } from "vitest";

import { machineColors } from "./machine-colors";

describe("machineColors", () => {
  it("gives a machine the same color whichever machines share its chart", () => {
    const aloneColor = (name: string) => machineColors([name], "light").get(name);
    const target = "runner-arm64";
    const others = ["runner-x86-64", "build-1", "mac-studio", "zz-last", "gpu-a100"]
      .filter((name) => aloneColor(name) !== aloneColor(target));
    expect(others.length).toBeGreaterThan(1);
    for (const other of others) {
      expect(machineColors([other, target], "light").get(target)).toBe(aloneColor(target));
    }
    expect(machineColors([...others, target], "light").get(target)).toBe(aloneColor(target));
  });

  it("gives every machine in a chart its own color up to the palette size", () => {
    const names = ["m1", "m2", "m3", "m4", "m5", "m6"];
    expect(new Set(machineColors(names, "light").values()).size).toBe(6);
    expect(new Set(machineColors(names, "dark").values()).size).toBe(6);
  });

  it("still colors every machine past the palette size", () => {
    const names = Array.from({ length: 9 }, (_, index) => `machine-${index}`);
    const colors = machineColors(names, "light");
    expect(names.every((name) => colors.get(name)?.startsWith("#"))).toBe(true);
  });

  it("uses a separate palette for the dark theme", () => {
    expect(machineColors(["m1"], "dark").get("m1")).not.toBe(machineColors(["m1"], "light").get("m1"));
  });
});
