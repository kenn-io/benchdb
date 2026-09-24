<script lang="ts">
  import { appURL } from "../base-path";
  import { createBenchDBClient } from "../api/client";
  import { formatBytes } from "../format";
  import type { ResultViewModel } from "../result/loader";

  let {
    result,
    baseUrl = "",
    canWrite = false,
  }: {
    result: Pick<ResultViewModel, "id" | "artifacts" | "diagnostics">;
    baseUrl?: string;
    canWrite?: boolean;
  } = $props();

  let removed = $state<string[]>([]);
  let busy = $state<string | null>(null);
  let actionError = $state<string | null>(null);
  let actionMessage = $state<string | null>(null);
  const artifacts = $derived(result.artifacts.filter((artifact) => !removed.includes(artifact.id)));

  async function deleteArtifact(id: string, name: string) {
    if (!window.confirm(`Delete attachment ${name}? The benchmark result will be kept.`)) return;
    busy = id;
    actionError = null;
    actionMessage = null;
    try {
      const response = await createBenchDBClient(baseUrl).deleteResultArtifact(result.id, id);
      if (response.status >= 400) {
        actionError = (response.data as unknown as { detail?: string }).detail ?? "Could not delete the attachment. Try again.";
      } else {
        removed = [...removed, id];
        actionMessage = `Deleted ${name}.`;
      }
    } catch {
      actionError = "Could not reach the server. Try again.";
    } finally {
      busy = null;
    }
  }

  const captureState = $derived(result.diagnostics?.["state"]);
  const failure = $derived(result.diagnostics?.["failure_class"]);
  const status = $derived(
    captureState === "complete" ? "Diagnostic capture complete."
      : captureState === "unsupported" ? "Worker profiles unavailable for this revision."
      : captureState === "failed" ? "Diagnostic capture failed; any available files are listed below."
      : captureState === "partial" ? "Diagnostic capture incomplete; available files are listed below."
      : artifacts.length > 0 ? "Diagnostic files available."
      : "No diagnostic files were attached to this result.",
  );
</script>

<div class="diagnostics">
  <p>{status}</p>
  {#if actionError}<p class="failure" role="alert">{actionError}</p>{/if}
  {#if actionMessage}<p role="status">{actionMessage}</p>{/if}
  {#if typeof failure === "string" && failure !== ""}
    <p class="failure">Reason: {failure}</p>
  {/if}
  {#if artifacts.length > 0}
    <ul aria-label="Diagnostic downloads">
      {#each artifacts as artifact (artifact.id)}
        <li>
          <a
            href={appURL(`${baseUrl.replace(/\/$/, "")}/api/benchmark-results/${encodeURIComponent(result.id)}/artifacts/${encodeURIComponent(artifact.id)}`)}
            download={artifact.name}
          >{artifact.name}</a>
          <span>{formatBytes(artifact.size_bytes)}</span>
          {#if canWrite}
            <button type="button" class="button-pill danger" disabled={busy !== null} onclick={() => deleteArtifact(artifact.id, artifact.name)} aria-label={`Delete ${artifact.name}`}>
              {busy === artifact.id ? "Deleting…" : "Delete"}
            </button>
          {/if}
        </li>
      {/each}
    </ul>
    {#if artifacts.some((artifact) => artifact.media_type === "application/vnd.google.pprof")}
      <p>Open CPU and memory profiles with <code>go tool pprof</code>.</p>
    {/if}
  {/if}
</div>

<style>
  .diagnostics { display: grid; gap: 8px; min-width: 0; font-size: 0.8125rem; }
  p { margin: 0; color: var(--c-text-muted); }
  .danger { color: var(--c-error); }
  .failure { overflow-wrap: anywhere; }
  ul { display: grid; gap: 4px; list-style: none; margin: 0; padding: 0; }
  li { display: flex; flex-wrap: wrap; align-items: baseline; gap: 4px 12px; }
  a { padding-block: 4px; overflow-wrap: anywhere; }
  li span { color: var(--c-text-muted); white-space: nowrap; font-variant-numeric: tabular-nums; }
</style>
