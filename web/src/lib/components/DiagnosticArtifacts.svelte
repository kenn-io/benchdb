<script lang="ts">
  import { formatBytes } from "../format";
  import type { ResultViewModel } from "../result/loader";

  let {
    result,
    baseUrl = "",
  }: {
    result: Pick<ResultViewModel, "id" | "artifacts" | "diagnostics">;
    baseUrl?: string;
  } = $props();

  const state = $derived(result.diagnostics?.["state"]);
  const failure = $derived(result.diagnostics?.["failure_class"]);
  const status = $derived(
    state === "complete" ? "Diagnostic capture complete."
      : state === "unsupported" ? "Worker profiles unavailable for this revision."
      : state === "failed" ? "Diagnostic capture failed; any available files are listed below."
      : state === "partial" ? "Diagnostic capture incomplete; available files are listed below."
      : result.artifacts.length > 0 ? "Diagnostic files available."
      : "No diagnostic files were attached to this result.",
  );
</script>

<div class="diagnostics">
  <p>{status}</p>
  {#if typeof failure === "string" && failure !== ""}
    <p class="failure">Reason: {failure}</p>
  {/if}
  {#if result.artifacts.length > 0}
    <ul aria-label="Diagnostic downloads">
      {#each result.artifacts as artifact (artifact.name)}
        <li>
          <a
            href={`${baseUrl.replace(/\/$/, "")}/api/benchmark-results/${encodeURIComponent(result.id)}/artifacts/${encodeURIComponent(artifact.name)}`}
            download={artifact.name}
          >{artifact.name}</a>
          <span>{formatBytes(artifact.size_bytes)}</span>
        </li>
      {/each}
    </ul>
    {#if result.artifacts.some((artifact) => artifact.media_type === "application/vnd.google.pprof")}
      <p>Open CPU and memory profiles with <code>go tool pprof</code>.</p>
    {/if}
  {/if}
</div>

<style>
  .diagnostics { display: grid; gap: 8px; min-width: 0; font-size: 0.8125rem; }
  p { margin: 0; color: var(--c-text-muted); }
  .failure { overflow-wrap: anywhere; }
  ul { display: grid; gap: 4px; list-style: none; margin: 0; padding: 0; }
  li { display: flex; flex-wrap: wrap; align-items: baseline; gap: 4px 12px; }
  a { padding-block: 4px; overflow-wrap: anywhere; }
  li span { color: var(--c-text-muted); white-space: nowrap; font-variant-numeric: tabular-nums; }
</style>
