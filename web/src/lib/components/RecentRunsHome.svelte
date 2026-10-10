<script lang="ts">
  import { appURL } from "../base-path";
  import { onMount, untrack } from "svelte";

  import { createBenchDBClient } from "../api/client";
  import {
    listRecentRuns,
    RECENT_RUNS_PAGE_SIZE,
    type RecentRunAttentionViewModel,
    type RecentRunViewModel,
  } from "../home/loader";
  import SearchIcon from "@lucide/svelte/icons/search";

  import { relativeTime } from "../format";
  import { repositoryLabel } from "../repository";
  import { DEFAULT_HOME_QUERY, formatHomeQuery, interceptNavClick, navigate, type HomeQuery } from "../router";

  let {
    baseUrl = "",
    query = DEFAULT_HOME_QUERY,
  }: {
    baseUrl?: string;
    query?: HomeQuery;
  } = $props();

  const client = $derived(createBenchDBClient(baseUrl));

  let search = $state(untrack(() => query.q));
  let hasMore = $state(false);
  let runs = $state<RecentRunViewModel[]>([]);
  let loading = $state(true);
  let errorMsg = $state<string | null>(null);

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    errorMsg = null;
    try {
      const page = await listRecentRuns(client, query);
      runs = page.runs;
      hasMore = page.hasMore;
    } catch (err) {
      errorMsg = err instanceof Error ? err.message : String(err);
    } finally {
      loading = false;
    }
  }

  const totalResults = $derived(runs.reduce((sum, run) => sum + run.resultCount, 0));
  const totalErrors = $derived(runs.reduce((sum, run) => sum + run.errorCount, 0));
  const machineNames = $derived(Array.from(new Set(runs.flatMap((run) => run.machineNames))).sort());
  const repositoryLabels = $derived(uniqueRepositoryLabels(runs));
  const showRepositoryColumn = $derived(repositoryLabels.length > 1);
  const attentionRuns = $derived(runs.filter((run) => run.attention !== null));
  // The server checks only the newest runs on each page for regressions
  // (recentRunsAttentionLimit), so the page says which runs were checked.
  const ATTENTION_WINDOW = 5;
  const checkedRuns = $derived(Math.min(runs.length, ATTENTION_WINDOW));
  const checkedText = $derived(
    checkedRuns === 1 ? "the newest run on this page" : `the newest ${checkedRuns} runs on this page`,
  );

  // Relative times advance while the page stays open.
  let now = $state(new Date());
  onMount(() => {
    const timer = setInterval(() => (now = new Date()), 60_000);
    return () => clearInterval(timer);
  });
  const selectedRepositoryLabel = $derived(
    query.repository === "" ? "All projects" : repositoryLabel(query.repository),
  );

  function go(e: MouseEvent, href: string) {
    if (!interceptNavClick(e)) return;
    e.preventDefault();
    navigate(href);
  }

  function formatTime(value: string): string {
    return new Intl.DateTimeFormat(undefined, {
      month: "short",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      hour12: false,
    }).format(new Date(value));
  }

  function plural(n: number, word: string, pluralWord = `${word}s`): string {
    return `${n.toLocaleString()} ${n === 1 ? word : pluralWord}`;
  }

  function uniqueRepositoryLabels(source: RecentRunViewModel[]): string[] {
    const labels = new Map<string, string>();
    for (const run of source) {
      if (run.repository === "") continue;
      labels.set(run.repository, run.repositoryLabel);
    }
    return Array.from(labels.values()).sort();
  }

  function submitSearch(e: SubmitEvent) {
    e.preventDefault();
    navigate(`/${formatHomeQuery({ repository: query.repository, q: search.trim() })}`);
  }

  function pageHref(offset: number): string {
    return `/${formatHomeQuery({ ...query, offset })}`;
  }

  function attentionStatusLabel(attention: RecentRunAttentionViewModel): string {
    return attention.status === "failure" ? "Regression" : "Action required";
  }

</script>

<main class="page home-page">
  <header class="page-header">
    <div>
      <p class="eyebrow">{selectedRepositoryLabel}</p>
      <h1>Runs</h1>
    </div>
    <form class="run-search" onsubmit={submitSearch} role="search" aria-label="Find a commit">
      <label class="sr-only" for="commit-search">Commit SHA or URL</label>
      <SearchIcon class="run-search-icon" size={14} strokeWidth={1.75} aria-hidden="true" />
      <input id="commit-search" type="search" bind:value={search} maxlength="2048"
        placeholder="Find a commit SHA or URL" />
      <button type="submit" class="sr-only">Search runs</button>
      {#if query.q}
        <a class="clear-search" href={appURL(`/${formatHomeQuery({ repository: query.repository })}`)}
          onclick={(e) => go(e, `/${formatHomeQuery({ repository: query.repository })}`)}>Clear</a>
      {/if}
    </form>
  </header>

  {#if errorMsg}
    <p class="error">Failed to load recent runs: {errorMsg}</p>
  {:else if loading}
    <p>Loading…</p>
  {:else if runs.length === 0}
    <section class="panel empty-panel">
      <h2>{query.q ? "No matching runs" : query.offset > 0 ? "No runs on this page" : "No runs yet"}</h2>
      {#if query.q}<p>Older commits match by full SHA or commit URL.</p>{/if}
    </section>
  {:else}
    <p class="summary-line" aria-label="Recent run summary">
      <span class="summary-item">{plural(runs.length, "run")}</span>
      <span class="summary-item">{plural(totalResults, "result")}</span>
      {#if machineNames.length > 0}<span class="summary-item">{plural(machineNames.length, "machine")}</span>{/if}
      {#if totalErrors > 0}
        <span class="summary-item alert">{plural(totalErrors, "error")}</span>
      {/if}
    </p>

    {#if attentionRuns.length === 0}
      <p class="attention-clear">Nothing needs attention in {checkedText}.</p>
    {:else}
      <section class="attention-panel" aria-labelledby="home-attention-heading">
        <h2 id="home-attention-heading">Needs attention <span>· {checkedText}</span></h2>
        <ul class="attention-list">
          {#each attentionRuns as run (run.runId)}
            {@const attention = run.attention!}
            <li>
              <a
                class="attention-link"
                href={appURL(attention.reportHref)}
                aria-label={`Review CI report for run ${run.runId}`}
                onclick={(e) => go(e, attention.reportHref)}
              >
                <span class={`attention-status ${attention.status}`}>{attentionStatusLabel(attention)}</span>
                <strong>{run.primaryLabel}</strong>
                <span class="attention-summary">{attention.summaryText}</span>
                <span class="attention-reason">{attention.statusReason}</span>
              </a>
            </li>
          {/each}
        </ul>
      </section>
    {/if}

    <section class="panel table-panel" aria-label="Benchmark runs">
      <table class="data-table stacked-table runs-table">
        <thead>
          <tr>
            <th>Commit</th>
            {#if showRepositoryColumn}<th>Project</th>{/if}
            <th>Run</th>
            <th>Results</th>
            <th>When</th>
          </tr>
        </thead>
        <tbody>
          {#each runs as run (run.runId)}
            <tr class:error-row={run.errorCount > 0} class:attention-row={run.attention !== null}>
              <td data-label="Commit">
                <div class="commit-cell">
                  <a
                    class="row-primary-link"
                    href={appURL(run.runHref)}
                    aria-label={`Open run ${run.runId}`}
                    onclick={(e) => go(e, run.runHref)}
                  >{run.primaryLabel}</a>
                  <div class="meta-line">
                    {#if run.commitHref && run.shortCommit}
                      <a
                        class="mono"
                        href={run.commitHref}
                        aria-label={`Open commit ${run.shortCommit} on GitHub`}
                        target="_blank"
                        rel="noreferrer"
                      >{run.shortCommit}</a>
                    {:else if run.shortCommit}
                      <span class="mono">{run.shortCommit}</span>
                    {/if}
                    {#if run.authorLabel !== "unknown author"}
                      <span class="author">
                        {#if run.authorAvatar}
                          <img src={run.authorAvatar} alt="" loading="lazy" referrerpolicy="no-referrer" />
                        {/if}
                        {run.authorLabel}
                      </span>
                    {/if}
                  </div>
                </div>
              </td>
              {#if showRepositoryColumn}
                <td data-label="Project">
                  <span title={run.repository || undefined}>{run.repository === "" ? "—" : run.repositoryLabel}</span>
                </td>
              {/if}
              <td data-label="Run">
                <div class="run-cell">
                  <div class="run-line">
                    {#if run.runReason}<span class="reason-chip">{run.runReason}</span>{/if}
                    <span>{run.machineLabel}</span>
                  </div>
                  <span class="muted-detail mono" title={run.runId}>{run.displayRunId}</span>
                  {#if run.latestBatchId && run.latestBatchHref}
                    <a
                      class="muted-detail batch-link"
                      href={appURL(run.latestBatchHref)}
                      aria-label={`Open batch ${run.latestBatchId}`}
                      title={run.latestBatchId}
                      onclick={(e) => go(e, run.latestBatchHref!)}
                    >batch {run.displayLatestBatchId}{run.batchCount > 1 ? ` +${run.batchCount - 1}` : ""}</a>
                  {/if}
                </div>
              </td>
              <td data-label="Results">
                <div class="count-stack">
                  <span>{plural(run.resultCount, "result")}</span>
                  {#if run.attention}
                    <span class={`attention-mini ${run.attention.status}`}>{run.attention.summaryText}</span>
                  {/if}
                  {#if run.errorCount > 0}
                    <span class="status-badge warning">{plural(run.errorCount, "error")}</span>
                  {/if}
                </div>
              </td>
              <td data-label="When">
                <time datetime={run.lastResultAt} title={formatTime(run.lastResultAt)}>{relativeTime(run.lastResultAt, now)}</time>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </section>
  {/if}
  {#if !loading && !errorMsg && (runs.length > 0 || query.offset > 0)}
    <nav class="run-pagination" aria-label="Run pages">
      {#if query.offset > 0}
        <a href={appURL(pageHref(Math.max(0, query.offset - RECENT_RUNS_PAGE_SIZE)))}
          onclick={(e) => go(e, pageHref(Math.max(0, query.offset - RECENT_RUNS_PAGE_SIZE)))}>Previous</a>
      {:else}<span aria-disabled="true">Previous</span>{/if}
      <span aria-live="polite">{runs.length ? `Runs ${query.offset + 1}–${query.offset + runs.length}` : "No more runs"}</span>
      {#if hasMore}
        <a href={appURL(pageHref(query.offset + RECENT_RUNS_PAGE_SIZE))}
          onclick={(e) => go(e, pageHref(query.offset + RECENT_RUNS_PAGE_SIZE))}>Next</a>
      {:else}<span aria-disabled="true">Next</span>{/if}
    </nav>
  {/if}
</main>

<style>
  .home-page {
    gap: 12px;
    height: 100%;
    min-height: 0;
    max-height: 100%;
  }
  .home-page .table-panel {
    flex: 1;
    min-height: 0;
    overflow: auto;
  }

  .run-search {
    position: relative;
    display: flex;
    align-items: center;
    gap: 8px;
    width: min(340px, 100%);
  }
  .run-search :global(.run-search-icon) {
    position: absolute;
    left: 9px;
    color: var(--c-text-faint);
    pointer-events: none;
  }
  .run-search input {
    flex: 1;
    min-width: 0;
    height: 32px;
    padding: 0 10px 0 28px;
    border: 1px solid var(--c-border);
    border-radius: var(--radius-md);
    background: var(--c-surface);
    color: var(--c-text);
    font: inherit;
    font-size: 0.82rem;
  }
  .run-search input::placeholder { color: var(--c-text-faint); }
  .run-search input:focus-visible {
    outline: none;
    border-color: var(--c-accent);
    box-shadow: 0 0 0 3px var(--c-focus-ring);
  }
  @media (forced-colors: active) {
    .run-search input:focus-visible { outline: 2px solid Highlight; outline-offset: 2px; }
  }
  .clear-search { font-size: 0.8rem; white-space: nowrap; }

  .attention-panel {
    display: grid;
    gap: 6px;
  }
  .attention-panel h2 {
    margin: 0;
    font-size: 0.82rem;
    font-weight: 700;
  }
  .attention-panel h2 span {
    color: var(--c-text-muted);
    font-weight: 500;
  }
  .attention-clear {
    margin: 0;
    color: var(--c-text-muted);
    font-size: 0.8rem;
  }
  .attention-list {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 360px), 1fr));
    gap: 8px;
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .attention-link {
    height: 100%;
    display: grid;
    grid-template-columns: auto minmax(0, 1fr) auto;
    align-items: baseline;
    gap: 4px 8px;
    padding: 9px 12px;
    border: 1px solid color-mix(in srgb, var(--c-error) 30%, var(--c-border-muted));
    border-left: 3px solid var(--c-error);
    border-radius: var(--radius-md);
    background: var(--c-surface);
    color: var(--c-text);
    text-decoration: none;
  }
  .attention-link:hover { background: var(--c-row-hover); }
  .attention-link strong {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .attention-status {
    color: var(--c-error);
    font-size: 0.72rem;
    font-weight: 750;
    text-transform: uppercase;
  }
  .attention-status.action_required { color: var(--c-warning); }
  .attention-summary { color: var(--c-error); font-size: 0.78rem; font-weight: 650; white-space: nowrap; }
  .attention-reason {
    grid-column: 1 / -1;
    color: var(--c-text-muted);
    font-size: 0.76rem;
  }

  .runs-table { --stacked-label-width: 80px; }
  .runs-table thead th { position: sticky; top: 0; z-index: 1; }
  .runs-table td { vertical-align: top; }
  .commit-cell,
  .run-cell,
  .count-stack {
    min-width: 0;
    display: grid;
    justify-items: start;
    gap: 3px;
  }
  .commit-cell .row-primary-link { font-weight: 650; }
  .meta-line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 3px 10px;
    color: var(--c-text-muted);
    font-size: 0.76rem;
  }
  .meta-line a {
    color: inherit;
    text-decoration: none;
  }
  .meta-line a:hover {
    color: var(--c-accent);
    text-decoration: underline;
  }
  .author {
    display: inline-flex;
    align-items: center;
    gap: 5px;
  }
  .author img {
    width: 16px;
    height: 16px;
    border-radius: 50%;
    background: var(--c-surface-subtle);
  }
  .run-line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 4px 8px;
  }
  .reason-chip {
    padding: 0 7px;
    border: 1px solid var(--c-border-muted);
    border-radius: 999px;
    background: var(--c-bg-inset);
    color: var(--c-text-muted);
    font-size: 0.72rem;
    line-height: 1.6;
  }
  .muted-detail {
    color: var(--c-text-faint);
    font-size: 0.72rem;
    overflow-wrap: anywhere;
  }
  .batch-link { text-decoration: none; }
  .batch-link:hover { color: var(--c-accent); }
  .attention-mini { color: var(--c-error); font-size: 0.74rem; font-weight: 700; }
  .attention-mini.action_required { color: var(--c-warning); }
  time { color: var(--c-text-muted); white-space: nowrap; }

  .run-pagination { display: flex; justify-content: space-between; align-items: center; gap: 12px; flex-shrink: 0; }
  .run-pagination a { padding: 8px; }
  .run-pagination [aria-disabled] { color: var(--c-text-muted); padding: 8px; }
</style>
