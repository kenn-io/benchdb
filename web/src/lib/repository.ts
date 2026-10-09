import type { createBenchDBClient } from "./api/client";

type Client = ReturnType<typeof createBenchDBClient>;

/** repositoryLabel shortens a repository URL for display: GitHub URLs become
 * owner/name, other URLs host/path, and unparseable values pass through. */
export function repositoryLabel(repository: string): string {
  if (repository === "") {
    return "not set";
  }
  let u: URL;
  try {
    u = new URL(repository);
  } catch {
    return repository;
  }
  const parts = u.pathname.split("/").filter(Boolean);
  if ((u.hostname === "github.com" || u.hostname === "www.github.com") && parts.length >= 2) {
    return `${parts[0]}/${parts[1]}`;
  }
  const path = u.pathname === "/" ? "" : u.pathname.replace(/\/$/, "");
  return `${u.hostname}${path}`;
}

/** listRepositories returns every repository with benchmark results, most
 * recently active first. Throws so the caller owns error presentation. */
export async function listRepositories(client: Client): Promise<string[]> {
  const res = await client.listRepositories();
  if (res.status >= 400 || !res.data) {
    throw new Error((res.data as { detail?: string } | undefined)?.detail ?? "failed to list repositories");
  }
  return (res.data.repositories ?? []).map((row) => row.repository);
}
