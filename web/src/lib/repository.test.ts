import { getBenchDB } from "./api/benchdb";
import type { AxiosInstance } from "axios";
import { describe, expect, it, vi } from "vitest";

import type { createBenchDBClient } from "./api/client";
import { listRepositories, repositoryLabel } from "./repository";

type Client = ReturnType<typeof createBenchDBClient>;

function fakeClient(response: { status: number; data: unknown }): Client {
  const get = vi.fn(async () => response);
  return getBenchDB({ get } as unknown as AxiosInstance) as unknown as Client;
}

describe("repositoryLabel", () => {
  it("shortens GitHub URLs to owner/name", () => {
    expect(repositoryLabel("https://github.com/apache/arrow")).toBe("apache/arrow");
    expect(repositoryLabel("https://github.com/apache/arrow/")).toBe("apache/arrow");
  });

  it("keeps the host and path for other URLs and passes other text through", () => {
    expect(repositoryLabel("https://gitlab.example.com/group/project/")).toBe("gitlab.example.com/group/project");
    expect(repositoryLabel("not a url")).toBe("not a url");
    expect(repositoryLabel("")).toBe("not set");
  });
});

describe("listRepositories", () => {
  it("returns repository URLs in server order", async () => {
    const client = fakeClient({
      status: 200,
      data: { repositories: [{ repository: "https://github.com/b/b" }, { repository: "https://github.com/a/a" }] },
    });
    await expect(listRepositories(client)).resolves.toEqual(["https://github.com/b/b", "https://github.com/a/a"]);
  });

  it("treats a null list as empty and surfaces endpoint errors", async () => {
    await expect(listRepositories(fakeClient({ status: 200, data: { repositories: null } }))).resolves.toEqual([]);
    await expect(listRepositories(fakeClient({ status: 500, data: { detail: "database unavailable" } })))
      .rejects.toThrow("database unavailable");
  });
});
