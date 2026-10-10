<script lang="ts">
  import { hasCIReportSelector, type CIReport } from "../ci-report/loader";
  import type { CIReportQuery } from "../router";
  import { repositoryLabel } from "../repository";
  import CIReportView from "./CIReportView.svelte";

  let {
    query,
    baseUrl = "",
  }: {
    query: CIReportQuery;
    baseUrl?: string;
  } = $props();

  const ready = $derived(hasCIReportSelector(query));
</script>

{#snippet header(r: CIReport)}
  <header class="page-header">
    <div>
      <p class="eyebrow">CI report</p>
      <h1>{r.repository ? repositoryLabel(r.repository) : "Run selection"}</h1>
    </div>
    <div class="page-meta">
      {#if r.commit_sha}<span class="mono" title={r.commit_sha}>commit {r.commit_sha.slice(0, 8)}</span>{/if}
      {#if r.evaluated_at}
        <span>Saved report · {new Date(r.evaluated_at).toLocaleString()}</span>
      {:else}
        <span>baseline {r.baseline.replaceAll("_", " ")}</span>
      {/if}
    </div>
  </header>
{/snippet}

<main class="page ci-report-page">
  {#if ready}
    <CIReportView {query} {baseUrl} {header} />
  {:else}
    <h1>CI report</h1>
    <p>Open a CI report URL with a commit selector or run IDs.</p>
  {/if}
</main>
