<script lang="ts">
  import { SearchInput } from "@kenn-io/kit-ui/search-input";
  import { SelectDropdown, type SelectDropdownOption } from "@kenn-io/kit-ui/select-dropdown";
  import { Toggle } from "@kenn-io/kit-ui/toggle";
  import { onDestroy } from "svelte";

  import { createBenchDBClient } from "../api/client";
  import { listSeries } from "../browse/loader";
  import { sortRows, type BrowseRow, type SortKey, type SortSpec } from "../browse/transform";
  import { formatBrowseQuery, interceptNavClick, navigate, type BrowseQuery, type BrowseWindow } from "../router";
  import BrowseTable from "./BrowseTable.svelte";
  import BrowseTrendCard from "./BrowseTrendCard.svelte";

  let {
    query,
    baseUrl = "",
  }: {
    query: BrowseQuery;
    baseUrl?: string;
  } = $props();

  // baseUrl is a fixed prop; $derived rebuilds the client only if it ever
  // changes, which silences Svelte's "captures the initial value" warning. The
  // load effect also tracks `client`, but it is referentially stable (fixed
  // `baseUrl` prop), so it never triggers a refetch.
  const client = $derived(createBenchDBClient(baseUrl));

  let rows = $state<BrowseRow[]>([]);
  let nextCursor = $state<string | null>(null);
  let loading = $state(true);
  let loadingMore = $state(false);
  let errorMsg = $state<string | null>(null);
  let moreErrorMsg = $state<string | null>(null);
  let sort = $state<SortSpec | null>(null);
  let searchFilter = $state("");
  let repositoryFilter = $state("");
  let exactFiltersOpen = $state(false);
  let chartView = $state(false);
  let searchTimer: ReturnType<typeof setTimeout> | undefined;
  // Monotonic token: a stale response (filters changed mid-flight) must not
  // overwrite a newer page.
  let reqToken = 0;

  $effect(() => {
    searchFilter = query.q;
    repositoryFilter = query.repository;
    void load(query);
  });

  async function load(q: BrowseQuery) {
    const token = ++reqToken;
    loading = true;
    rows = [];
    nextCursor = null;
    // A fresh page load supersedes any in-flight load-more, whose guarded
    // finally will then skip its cleanup — clear the flag here so the new
    // page's Load more cannot start out wedged disabled.
    loadingMore = false;
    errorMsg = null;
    moreErrorMsg = null;
    try {
      const page = await listSeries(client, q);
      if (token !== reqToken) return;
      rows = page.rows;
      nextCursor = page.nextCursor;
    } catch (err) {
      if (token !== reqToken) return;
      errorMsg = err instanceof Error ? err.message : String(err);
    } finally {
      if (token === reqToken) loading = false;
    }
  }

  async function loadMore() {
    if (nextCursor === null || loadingMore) return;
    const token = reqToken;
    loadingMore = true;
    moreErrorMsg = null;
    try {
      const page = await listSeries(client, query, nextCursor);
      if (token !== reqToken) return;
      rows = [...rows, ...page.rows];
      nextCursor = page.nextCursor;
    } catch (err) {
      if (token !== reqToken) return;
      moreErrorMsg = err instanceof Error ? err.message : String(err);
    } finally {
      if (token === reqToken) loadingMore = false;
    }
  }

  function setFilter(patch: Partial<BrowseQuery>) {
    navigate(`/series${formatBrowseQuery({ ...query, ...patch })}`);
  }

  function clearFilter(patch: Partial<BrowseQuery>) {
    setFilter(patch);
  }

  function submitExactFilters(e: SubmitEvent) {
    e.preventDefault();
    setFilter({
      repository: repositoryFilter.trim(),
    });
  }

  function updateSearch(value: string) {
    searchFilter = value;
    if (searchTimer !== undefined) clearTimeout(searchTimer);
    searchTimer = setTimeout(() => {
      const q = value.trim();
      if (q !== query.q) setFilter({ q });
    }, 250);
  }

  function clearSearch() {
    if (searchTimer !== undefined) clearTimeout(searchTimer);
    if (query.q !== "") setFilter({ q: "" });
  }

  onDestroy(() => {
    if (searchTimer !== undefined) clearTimeout(searchTimer);
  });

  function toggleSort(key: SortKey) {
    if (sort?.key !== key) {
      sort = { key, dir: "asc" };
    } else if (sort.dir === "asc") {
      sort = { key, dir: "desc" };
    } else {
      sort = null;
    }
  }

  function open(row: BrowseRow) {
    navigate(`/series/${row.benchmarkId}`);
  }

  function go(e: MouseEvent, href: string) {
    if (!interceptNavClick(e)) return;
    e.preventDefault();
    navigate(href);
  }

  let visible = $derived(sortRows(rows, sort));
  let machineOptions = $derived.by((): SelectDropdownOption[] => {
    const names = new Set(rows.flatMap((row) => row.machineNames));
    if (query.hardware !== "") names.add(query.hardware);
    return [
      { value: "", label: "All machines" },
      ...[...names].sort().map((name) => ({ value: name, label: name })),
    ];
  });
  const windowLabel: Record<BrowseWindow, string> = {
    all: "all time",
    "30d": "last 30 days",
    "3mo": "last 3 months",
    "1y": "last year",
  };
  const windowOptions: { value: BrowseWindow; label: string }[] = [
    { value: "all", label: "All time" },
    { value: "30d", label: "Last 30 days" },
    { value: "3mo", label: "Last 3 months" },
    { value: "1y", label: "Last year" },
  ];
  let activeFilters = $derived([
    ...(query.q !== ""
      ? [{ label: "query", value: query.q, clear: { q: "" }, aria: `Remove query filter ${query.q}` }]
      : []),
    ...(query.hardware !== ""
      ? [{ label: "machine", value: query.hardware, clear: { hardware: "" }, aria: `Remove machine filter ${query.hardware}` }]
      : []),
    ...(query.repository !== ""
      ? [{
          label: "repository",
          value: query.repository,
          clear: { repository: "" },
          aria: `Remove repository filter ${query.repository}`,
        }]
      : []),
    ...(query.window !== "all"
      ? [{
          label: "window",
          value: windowLabel[query.window],
          clear: { window: "all" as const },
          aria: `Remove window filter ${windowLabel[query.window]}`,
        }]
      : []),
  ]);
  let loadedSummary = $derived(
    `Showing ${visible.length} loaded ${visible.length === 1 ? "series" : "series"}`,
  );
</script>

<main class="page series-page">
  <header class="page-header">
    <div>
      <p class="eyebrow">Benchmark Explorer</p>
      <h1>Benchmark series</h1>
      <p class="page-subtitle">
        Scan benchmark families, current status, recent history, and default-branch coverage.
      </p>
    </div>
    <div class="header-actions">
      <div class="page-meta">
        <span>{loadedSummary}</span>
        {#if nextCursor !== null}<span>More available</span>{/if}
      </div>
      <a class="button-pill secondary" href="/results" onclick={(e) => go(e, "/results")}>Result explorer</a>
    </div>
  </header>

  <div class="panel browse-filters">
    <div class="primary-filters">
      <div class="benchmark-search">
        <SearchInput
          bind:value={searchFilter}
          placeholder="Search benchmarks…"
          ariaLabel="Search benchmarks"
          oninput={updateSearch}
          onclear={clearSearch}
          block
        />
      </div>
      <label class="filter-label machine-select">
        Machine
        <SelectDropdown
          value={query.hardware}
          options={machineOptions}
          title="Machine"
          onchange={(hardware) => setFilter({ hardware })}
        />
      </label>
      <div class="filter-row" role="group" aria-label="Series time window">
        <span class="filter-row-label">Window</span>
        <div class="segmented-control">
          {#each windowOptions as option}
            <button
              type="button"
              class:active={query.window === option.value}
              aria-pressed={query.window === option.value}
              onclick={() => setFilter({ window: option.value })}
            >
              {option.label}
            </button>
          {/each}
        </div>
      </div>
      <Toggle checked={chartView} label="Trend charts" onchange={(checked) => (chartView = checked)} />
    </div>
    {#if activeFilters.length > 0}
      <button type="button" class="button-pill secondary" onclick={() => navigate("/series")}>Clear filters</button>
    {/if}
  </div>

  <div class="filter-toolbar">
    <button
      type="button"
      class="button-pill secondary"
      aria-expanded={exactFiltersOpen}
      onclick={() => (exactFiltersOpen = !exactFiltersOpen)}
    >
      Advanced filters
    </button>
  </div>

  {#if exactFiltersOpen}
    <section class="panel filter-disclosure" aria-label="Advanced series filters">
      <form class="exact-filter-form" onsubmit={submitExactFilters}>
        <label class="filter-label">
          Repository URL
          <input
            type="url"
            bind:value={repositoryFilter}
            placeholder="https://github.com/apache/arrow"
            autocomplete="off"
          />
        </label>
        <div class="filter-actions">
          <button type="submit" class="button-pill">Apply advanced filters</button>
          <a class="button-pill secondary" href="/series" onclick={(e) => go(e, "/series")}>Clear</a>
        </div>
      </form>
    </section>
  {/if}

  {#if activeFilters.length > 0}
    <div class="active-filters" role="group" aria-label="Active filters">
      {#each activeFilters as filter (filter)}
        <button type="button" class="filter-chip" aria-label={filter.aria} onclick={() => clearFilter(filter.clear)}>
          <span class="chip-label">{filter.label}</span>
          <span class="chip-value">{filter.value}</span>
          <span class="chip-x" aria-hidden="true">&times;</span>
        </button>
      {/each}
    </div>
  {/if}

  {#if errorMsg}
    <section class="panel state-panel error-panel" role="alert">
      <h2>Failed to load series</h2>
      <p>{errorMsg}</p>
    </section>
  {:else if loading}
    <section class="panel state-panel loading-panel" aria-live="polite">
      <h2>Loading benchmark series</h2>
      <p>Loading...</p>
    </section>
  {:else if visible.length === 0}
    <section class="panel state-panel empty-panel" aria-label="No matching benchmark series">
      <h2>No series match the current filters</h2>
      <p>Clear active filters or use the global search to open a benchmark family.</p>
    </section>
  {:else}
    {#if chartView}
      <section class="trend-grid" aria-label="Benchmark trend cards">
        {#each visible as row (row.benchmarkId)}
          <BrowseTrendCard {row} onopen={open} />
        {/each}
      </section>
    {:else}
      <BrowseTable rows={visible} {sort} onsort={toggleSort} onopen={open} />
    {/if}
    {#if sort !== null}
      <p class="scope-note">Sorting applies to loaded rows. Load more for a broader local sort.</p>
    {/if}
    {#if moreErrorMsg}
      <section class="panel state-panel error-panel" role="alert">
        <h2>Failed to load more</h2>
        <p>{moreErrorMsg}</p>
      </section>
    {/if}
    {#if nextCursor !== null}
      <button type="button" class="button-pill more" onclick={loadMore} disabled={loadingMore}>
        {loadingMore ? "Loading…" : "Load more"}
      </button>
    {/if}
  {/if}
</main>

<style>
  .series-page {
    gap: 12px;
  }
  .header-actions {
    display: grid;
    justify-items: end;
    gap: 8px;
  }
  @media (max-width: 760px) {
    .header-actions { justify-items: start; }
  }
  .browse-filters {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    padding: 10px 12px;
  }

  .primary-filters {
    display: flex;
    flex-wrap: wrap;
    align-items: end;
    gap: 10px;
  }

  .benchmark-search { width: min(320px, 100%); }
  .machine-select { min-width: 180px; }
  .machine-select :global(.kit-select-dropdown__trigger) { width: 100%; }
  .trend-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 420px), 1fr));
    gap: 10px;
  }

  .filter-row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }

  .filter-row-label {
    color: var(--c-text-muted);
    font-size: 0.72rem;
    font-weight: 700;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }

  .segmented-control {
    display: inline-flex;
    flex-wrap: wrap;
    gap: 4px;
    padding: 2px;
    border: 1px solid var(--c-border-muted);
    border-radius: var(--radius-md);
    background: var(--c-bg-inset);
  }

  .segmented-control button {
    min-height: 26px;
    padding: 0 9px;
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--c-text-muted);
    cursor: pointer;
  }

  .segmented-control button:hover {
    background: var(--c-surface-hover);
    color: var(--c-text);
  }

  .segmented-control button.active {
    background: var(--c-accent);
    color: var(--c-on-accent);
  }

  .filter-toolbar {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
  }

  .filter-disclosure {
    padding: 0;
  }

  .exact-filter-form {
    display: grid;
    grid-template-columns: repeat(2, minmax(180px, 1fr)) auto;
    gap: 10px;
    align-items: end;
    padding: 0 12px 12px;
  }

  .error-panel h2 {
    color: var(--c-error);
  }

  .scope-note {
    color: var(--c-text-muted);
    font-size: 0.78rem;
    margin: -2px 0 0;
  }
  .more {
    width: fit-content;
    margin-top: 0.25rem;
  }
  @media (max-width: 760px) {
    .browse-filters {
      align-items: stretch;
      flex-direction: column;
    }

    .exact-filter-form {
      grid-template-columns: 1fr;
    }
  }
</style>
