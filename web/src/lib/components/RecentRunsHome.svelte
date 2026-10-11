<script lang="ts">
  import { appURL } from "../base-path";
  import { onMount, untrack } from "svelte";

  import { createBenchDBClient } from "../api/client";
  import {
    listRecentRuns,
    RECENT_RUNS_PAGE_SIZE,
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
  let attentionRuns = $state(0);
  let verdictsPending = $state(false);
  let runs = $state<RecentRunViewModel[]>([]);
  let loading = $state(true);
  let errorMsg = $state<string | null>(null);
  let refreshError = $state<string | null>(null);

  // The server computes verdicts in the background; while any could still
  // change, reload a few times so new verdicts appear.
  const PENDING_REFRESH_MS = 4000;
  const PENDING_REFRESH_LIMIT = 8;
  let pendingRefreshes = 0;
  let refreshTimer: ReturnType<typeof setTimeout> | undefined;

  onMount(() => {
    void load();
    return () => clearTimeout(refreshTimer);
  });

  async function load(background = false) {
    if (!background) {
      loading = true;
      errorMsg = null;
    }
    try {
      const page = await listRecentRuns(client, query);
      runs = page.runs;
      hasMore = page.hasMore;
      attentionRuns = page.attentionRuns;
      verdictsPending = page.verdictsPending;
      refreshError = null;
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      // A failed refresh keeps the runs already shown.
      if (background) refreshError = message;
      else errorMsg = message;
    } finally {
      loading = false;
    }
    if (errorMsg === null) scheduleRefresh();
  }

  function scheduleRefresh() {
    clearTimeout(refreshTimer);
    const settled = refreshError === null && !verdictsPending && runs.every((run) => run.attentionChecked);
    if (settled || pendingRefreshes >= PENDING_REFRESH_LIMIT) return;
    pendingRefreshes += 1;
    refreshTimer = setTimeout(() => void load(true), PENDING_REFRESH_MS);
  }

  const totalResults = $derived(runs.reduce((sum, run) => sum + run.resultCount, 0));
  const totalErrors = $derived(runs.reduce((sum, run) => sum + run.errorCount, 0));
  const machineNames = $derived(Array.from(new Set(runs.flatMap((run) => run.machineNames))).sort());
  const repositoryLabels = $derived(uniqueRepositoryLabels(runs));
  const showRepositoryColumn = $derived(repositoryLabels.length > 1);
  const allRunsHref = $derived(`/${formatHomeQuery({ repository: query.repository, q: query.q })}`);
  const attentionHref = $derived(`/${formatHomeQuery({ repository: query.repository, q: query.q, attention: true })}`);

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
    navigate(`/${formatHomeQuery({ repository: query.repository, q: search.trim(), attention: query.attention })}`);
  }

  function pageHref(offset: number): string {
    return `/${formatHomeQuery({ ...query, offset })}`;
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
        <a class="clear-search"
          href={appURL(`/${formatHomeQuery({ repository: query.repository, attention: query.attention })}`)}
          onclick={(e) => go(e, `/${formatHomeQuery({ repository: query.repository, attention: query.attention })}`)}
        >Clear</a>
      {/if}
    </form>
  </header>

  {#if attentionRuns > 0 || query.attention}
    <nav class="run-filters" aria-label="Filter runs">
      <a class="button-pill" class:active={!query.attention} aria-current={query.attention ? undefined : "true"}
        href={appURL(allRunsHref)} onclick={(e) => go(e, allRunsHref)}>All runs</a>
      <a class="button-pill" class:active={query.attention} aria-current={query.attention ? "true" : undefined}
        href={appURL(attentionHref)} onclick={(e) => go(e, attentionHref)}>
        Needs attention <strong class="filter-count">{attentionRuns.toLocaleString()}</strong>
      </a>
    </nav>
  {/if}

  {#if errorMsg}
    <p class="error">Failed to load recent runs: {errorMsg}</p>
  {:else if loading}
    <p>Loading…</p>
  {:else if runs.length === 0}
    <section class="panel empty-panel">
      {#if query.attention}
        <h2>{query.offset > 0 ? "No runs on this page" : "Nothing needs attention"}</h2>
        {#if verdictsPending}<p>Some runs are still being checked.</p>{/if}
        <a href={appURL(allRunsHref)} onclick={(e) => go(e, allRunsHref)}>Show all runs</a>
      {:else}
        <h2>{query.q ? "No matching runs" : query.offset > 0 ? "No runs on this page" : "No runs yet"}</h2>
        {#if query.q}<p>Older commits match by full SHA or commit URL.</p>{/if}
      {/if}
    </section>
  {:else}
    <p class="summary-line" aria-label="Recent run summary">
      <span class="summary-item">{plural(runs.length, "run")}</span>
      <span class="summary-item">{plural(totalResults, "result")}</span>
      {#if machineNames.length > 0}<span class="summary-item">{plural(machineNames.length, "machine")}</span>{/if}
      {#if totalErrors > 0}
        <span class="summary-item alert">{plural(totalErrors, "error")}</span>
      {/if}
      {#if attentionRuns === 0 && !query.attention && !verdictsPending}
        <span class="summary-item">Nothing needs attention</span>
      {/if}
    </p>
    {#if refreshError}
      <p class="refresh-error" role="status">Couldn't refresh run verdicts: {refreshError}</p>
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
            <tr class:error-row={run.errorCount > 0}>
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
                    {@const attention = run.attention}
                    <a class={`verdict-link ${attention.status}`} href={appURL(attention.reportHref)}
                      title={attention.statusReason} onclick={(e) => go(e, attention.reportHref)}
                    >{attention.summaryText}<span class="sr-only">, CI report</span></a>
                  {/if}
                  {#if !run.attentionChecked}
                    <span class="checking">Checking…</span>
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
    flex: 0 1 auto;
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

  .run-filters {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .filter-count {
    margin-left: 6px;
    font-variant-numeric: tabular-nums;
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
  .verdict-link {
    display: inline-flex;
    align-items: center;
    min-height: 22px;
    padding: 0 8px;
    border: 1px solid color-mix(in srgb, var(--c-error) 45%, var(--c-border));
    border-radius: 999px;
    background: var(--c-error-soft);
    color: var(--c-text);
    font-size: 0.74rem;
    font-weight: 700;
    text-decoration: none;
  }
  .verdict-link.action_required {
    border-color: color-mix(in srgb, var(--c-warning) 45%, var(--c-border));
    background: var(--c-warning-soft);
    color: var(--c-warn-text);
  }
  .verdict-link:hover { border-color: var(--c-accent); }
  .checking { color: var(--c-text-muted); font-size: 0.74rem; }
  .refresh-error { margin: 0; color: var(--c-text-muted); font-size: 0.78rem; }
  time { color: var(--c-text-muted); white-space: nowrap; }

  .run-pagination { display: flex; justify-content: space-between; align-items: center; gap: 12px; flex-shrink: 0; }
  .run-pagination a { padding: 8px; }
  .run-pagination [aria-disabled] { color: var(--c-text-muted); padding: 8px; }
</style>
