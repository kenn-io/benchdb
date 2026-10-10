<script lang="ts">
  import { appURL } from "../base-path";
  import { onMount } from "svelte";

  import { createBenchDBClient } from "../api/client";
  import { formatMeasurement } from "../format";
  import { loadRunPage, type RunPageViewModel, type RunResultRow } from "../run/loader";
  import { interceptNavClick, navigate, type CIReportQuery } from "../router";
  import CIReportView from "./CIReportView.svelte";

  let {
    runId,
    baseUrl = "",
  }: {
    runId: string;
    baseUrl?: string;
  } = $props();

  const client = $derived(createBenchDBClient(baseUrl));

  let vm = $state<RunPageViewModel | null>(null);
  let loading = $state(true);
  let loadingMore = $state(false);
  let errorMsg = $state<string | null>(null);
  let moreErrorMsg = $state<string | null>(null);

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    errorMsg = null;
    try {
      vm = await loadRunPage(client, runId);
    } catch (err) {
      errorMsg = err instanceof Error ? err.message : String(err);
    } finally {
      loading = false;
    }
  }

  async function loadMore() {
    if (vm === null || vm.nextCursor === null || loadingMore) return;
    loadingMore = true;
    moreErrorMsg = null;
    try {
      const page = await loadRunPage(client, runId, vm.nextCursor);
      const rows = [...vm.rows, ...page.rows];
      vm = {
        ...vm,
        rows,
        loadedResults: vm.loadedResults + page.loadedResults,
        loadedErrors: vm.loadedErrors + page.loadedErrors,
        firstLoadedAt: rows.map((row) => row.timestamp).sort()[0] ?? null,
        nextCursor: page.nextCursor,
      };
    } catch (err) {
      moreErrorMsg = err instanceof Error ? err.message : String(err);
    } finally {
      loadingMore = false;
    }
  }



  function go(e: MouseEvent, href: string) {
    if (!interceptNavClick(e)) return;
    e.preventDefault();
    navigate(href);
  }

  function goCIReport(e: MouseEvent) {
    if (vm?.ciReportHref) {
      go(e, vm.ciReportHref);
    }
  }

  // The same comparison the Runs page links to: this run against the
  // default-branch run at its fork point. Without commit metadata the server
  // infers the baseline from the run alone.
  function reportQuery(model: RunPageViewModel): CIReportQuery {
    const byCommit = model.repository !== "" && model.commitSha !== null;
    return {
      saved: false,
      repository: byCommit ? model.repository : "",
      commit: byCommit ? model.commitSha! : "",
      runIDs: model.runId,
      baselineRunIDs: "",
      baseline: byCommit ? "fork_point" : "",
      threshold: "",
      thresholdZ: "",
    };
  }

  function formatTime(value: string | null): string {
    if (value === null) return "not set";
    return new Intl.DateTimeFormat(undefined, {
      month: "short",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      hour12: false,
    }).format(new Date(value));
  }

  function formatSVS(row: RunResultRow): string {
    return formatMeasurement(row.singleValueSummary, row.unit, "not computed");
  }

  function plural(n: number, word: string, pluralWord = `${word}s`): string {
    return `${n.toLocaleString()} ${n === 1 ? word : pluralWord}`;
  }

  function tagSummary(tags: Record<string, unknown>): string[] {
    const priority = ["query_id", "suite", "dataset", "scale_factor", "format", "language", "engine"];
    return Object.entries(tags)
      .filter(([, value]) => value !== null && value !== undefined && value !== "")
      .sort(([left], [right]) => {
        const leftIndex = priority.indexOf(left);
        const rightIndex = priority.indexOf(right);
        if (leftIndex >= 0 || rightIndex >= 0) {
          return (leftIndex < 0 ? priority.length : leftIndex) - (rightIndex < 0 ? priority.length : rightIndex);
        }
        return left.localeCompare(right);
      })
      .slice(0, 4)
      .map(([key, value]) => `${key} ${String(value)}`);
  }
</script>

{#if errorMsg}
  <main class="page run-page"><p class="error">Failed to load run: {errorMsg}</p></main>
{:else if loading || vm === null}
  <main class="page run-page"><p>Loading…</p></main>
{:else if vm.rows.length === 0}
  <main class="page run-page">
    <header class="page-header">
      <div>
        <p class="eyebrow">Run Detail</p>
        <h1>Run <span class="id-heading mono">{runId}</span></h1>
      </div>
    </header>
    <section class="panel empty-panel">
      <h2>No results found for this run</h2>
      <p>Check the run_id or open the recent-runs dashboard.</p>
      <a href={appURL("/")} onclick={(e) => go(e, "/")}>Recent runs</a>
    </section>
  </main>
{:else}
  {@const showBatch = vm.rows.some((row) => row.batchId !== null)}
  {@const showErrors = vm.loadedErrors > 0}
  <main class="page run-page">
    <header class="page-header">
      <div>
        <p class="eyebrow">{vm.repositoryLabel}</p>
        <h1>{vm.primaryLabel}</h1>
        <p class="page-subtitle run-subtitle">
          {#if vm.authorAvatar}
            <img class="author-avatar" src={vm.authorAvatar} alt="" loading="lazy" referrerpolicy="no-referrer" />
          {/if}
          {#if vm.authorLabel !== "unknown author"}
            <span>{vm.authorLabel}{vm.authorLogin ? ` @${vm.authorLogin}` : ""}</span>
          {/if}
          <span class="mono" title={vm.runId}>{vm.secondaryLabel}</span>
          <span>{formatTime(vm.firstLoadedAt)}</span>
        </p>
      </div>
      <div class="page-meta">
        {#if vm.runReason}<span class="wrap-anywhere">{vm.runReason}</span>{/if}
        {#if vm.commitHref && vm.shortCommit}
          <a
            class="mono wrap-anywhere"
            href={vm.commitHref}
            aria-label={`Open commit ${vm.shortCommit} on GitHub`}
            target="_blank"
            rel="noreferrer"
          >{vm.shortCommit}</a>
        {:else if vm.shortCommit}
          <span class="mono wrap-anywhere">{vm.shortCommit}</span>
        {/if}
        {#if vm.ciReportHref}
          <a
            href={appURL(vm.ciReportHref)}
            aria-label={`Open CI report for run ${vm.runId}`}
            onclick={goCIReport}
          >CI report</a>
        {/if}
      </div>
    </header>

    <CIReportView query={reportQuery(vm)} {baseUrl} />

    <details class="panel results-panel">
      <summary>
        <strong>Results</strong>
        <span>{plural(vm.loadedResults, "result")}{vm.nextCursor === null ? "" : "+"}</span>
        {#if showErrors}<span class="alert">{plural(vm.loadedErrors, "error")}</span>{/if}
      </summary>
      <table class="data-table stacked-table run-results-table">
        <thead>
          <tr>
            <th>Benchmark</th>
            <th>Measurement</th>
            {#if showErrors}<th>Status</th>{/if}
            {#if showBatch}<th>Batch</th>{/if}
            <th>Open</th>
          </tr>
        </thead>
        <tbody>
          {#each vm.rows as row (row.id)}
            <tr class:error-row={row.hasError}>
              <td data-label="Benchmark">
                <a
                  class="row-primary-link"
                  href={appURL(row.resultHref)}
                  aria-label={`Open result ${row.id} for ${row.benchmarkName}`}
                  onclick={(e) => go(e, row.resultHref)}
                >{row.benchmarkName}</a>
                {#if tagSummary(row.benchmarkTags).length > 0}
                  <div class="row-metadata">
                    {#each tagSummary(row.benchmarkTags) as tag}
                      <span class="tag-chip">{tag}</span>
                    {/each}
                  </div>
                {/if}
              </td>
              <td data-label="Measurement" class="numeric" title={row.singleValueSummaryType}>
                {formatSVS(row)}
              </td>
              {#if showErrors}
                <td data-label="Status">
                  <span class={`status-badge ${row.hasError ? "warning" : "success"}`}>
                    {row.hasError ? "error" : "ok"}
                  </span>
                </td>
              {/if}
              {#if showBatch}
                <td data-label="Batch">
                  {#if row.batchId && row.batchHref}
                    <a
                      class="mono"
                      href={appURL(row.batchHref)}
                      aria-label={`Open batch ${row.batchId}`}
                      title={row.batchId}
                      onclick={(e) => go(e, row.batchHref!)}
                    >
                      {row.displayBatchId}
                    </a>
                  {/if}
                </td>
              {/if}
              <td data-label="Open">
                {#if row.trendHref !== null}
                  {@const trendHref = row.trendHref}
                  <a
                    class="inline-action-link"
                    href={appURL(trendHref)}
                    aria-label={`Open series trend for ${row.benchmarkName} result ${row.id}`}
                    onclick={(e) => go(e, trendHref)}
                  >Trend</a>
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    {#if moreErrorMsg}
      <p class="error">Failed to load more: {moreErrorMsg}</p>
    {/if}
    {#if vm.nextCursor !== null}
      <button type="button" class="button-pill more" onclick={loadMore} disabled={loadingMore}>
        {loadingMore ? "Loading…" : "Load more"}
      </button>
    {/if}
    </details>
  </main>
{/if}

<style>
  .run-page {
    gap: 12px;
  }
  .id-heading {
    overflow-wrap: anywhere;
  }
  .run-subtitle {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px 12px;
  }
  .author-avatar {
    width: 20px;
    height: 20px;
    border-radius: 50%;
    background: var(--c-surface-subtle);
  }
  .results-panel {
    padding: 0;
    overflow: hidden;
  }
  .results-panel summary {
    display: flex;
    align-items: baseline;
    gap: 10px;
    padding: 10px 12px;
    cursor: pointer;
  }
  .results-panel summary span {
    color: var(--c-text-muted);
    font-size: 0.8rem;
  }
  .results-panel summary .alert {
    color: var(--c-error);
  }
  .results-panel .more {
    margin: 10px 12px 12px;
  }
  .row-metadata {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 4px 8px;
    min-width: 0;
    margin-top: 4px;
  }
  .tag-chip {
    color: var(--c-text-muted);
    background: var(--c-surface-subtle);
    border: 1px solid var(--c-border);
    border-radius: 999px;
    padding: 1px 6px;
    font-size: 0.68rem;
    line-height: 1.35;
  }
</style>
