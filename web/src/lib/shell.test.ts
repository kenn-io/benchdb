import { describe, expect, it } from "vitest";

import { shellQuote } from "./shell";

describe("shellQuote", () => {
  it("leaves ordinary server URLs bare", () => {
    expect(shellQuote("https://bench.example.com/benchdb")).toBe("https://bench.example.com/benchdb");
    expect(shellQuote("http://127.0.0.1:8080")).toBe("http://127.0.0.1:8080");
  });

  it.each([
    ["https://host/tools/bench&dev", "'https://host/tools/bench&dev'"],
    ["https://host/a b", "'https://host/a b'"],
    ["https://host/$(echo injected)", "'https://host/$(echo injected)'"],
    ["https://host/`id`;echo injected", "'https://host/`id`;echo injected'"],
    ["https://host/a\"b\\c*?", "'https://host/a\"b\\c*?'"],
    ["", "''"],
  ])("single-quotes %j, where the shell expands nothing", (value, quoted) => {
    expect(shellQuote(value)).toBe(quoted);
  });

  it("closes, escapes, and reopens around an embedded single quote", () => {
    expect(shellQuote("https://host/it's")).toBe("'https://host/it'\\''s'");
  });
});
