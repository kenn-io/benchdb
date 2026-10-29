<script lang="ts">
  import { PageHeader } from "@kenn-io/kit-ui";
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
  <PageHeader title={r.repository ? repositoryLabel(r.repository) : "Run selection"}>
    {#snippet before()}<p class="eyebrow">CI report</p>{/snippet}
    {#snippet actions()}
      <div class="page-meta">
        {#if r.commit_sha}<span class="mono" title={r.commit_sha}>commit {r.commit_sha.slice(0, 8)}</span>{/if}
        {#if r.evaluated_at}
          <span>Saved report{r.evaluation ? ` · Evaluation ${r.evaluation}` : ""} · {new Date(r.evaluated_at).toLocaleString()}</span>
        {:else}
          <span>baseline {r.baseline.replaceAll("_", " ")}</span>
        {/if}
      </div>
    {/snippet}
  </PageHeader>
{/snippet}

<main class="page ci-report-page">
  {#if ready}
    <CIReportView {query} {baseUrl} {header} />
  {:else}
    <h1>CI report</h1>
    <p>Open a CI report URL with a commit selector or run IDs.</p>
  {/if}
</main>
