<script lang="ts">
  import { appURL } from "../base-path";
  import { SearchInput } from "@kenn-io/kit-ui/search-input";
  import { SelectDropdown, type SelectDropdownOption } from "@kenn-io/kit-ui/select-dropdown";
  import { onDestroy } from "svelte";

  import { createBenchDBClient } from "../api/client";
  import { listSeries } from "../browse/loader";
  import {
    groupByRepository,
    sortRows,
    statusCounts,
    windowStartIso,
    type BrowseRow,
    type SortKey,
    type SortSpec,
  } from "../browse/transform";
  import { repositoryLabel } from "../repository";
  import { DEFAULT_BROWSE_QUERY, formatBrowseQuery, parseBrowseQuery, interceptNavClick, navigate, type BrowseQuery, type BrowseWindow } from "../router";
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
  const chartView = $derived(query.view === "charts");
  // Clearing filters keeps the project and the view.
  const clearedURL = $derived(
    `/series${formatBrowseQuery({ ...DEFAULT_BROWSE_QUERY, repository: query.repository, view: query.view })}`,
  );
  const hasFilters = $derived(query.q !== "" || query.hardware !== "" || query.window !== "all");
  // View-only navigation keeps the loaded rows and pagination cursor.
  const filterQuery = $derived(formatBrowseQuery({ ...query, view: "table" }));
  let zeroBased = $state(false);
  const timeRange = $derived.by(() => {
    const now = new Date();
    const since = windowStartIso(query.window, now);
    return since === null ? null : { min: Date.parse(since), max: now.getTime() };
  });
  let searchTimer: ReturnType<typeof setTimeout> | undefined;
  // Monotonic token: a stale response (filters changed mid-flight) must not
  // overwrite a newer page.
  let reqToken = 0;

  $effect(() => {
    const filters = parseBrowseQuery(filterQuery);
    searchFilter = filters.q;
    void load(filters);
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
    navigate(`/benchmarks/${row.benchmarkId}`);
  }

  function go(e: MouseEvent, href: string) {
    if (!interceptNavClick(e)) return;
    e.preventDefault();
    navigate(href);
  }

  const visible = $derived(sortRows(rows, sort));
  // With every project in view, each project gets its own section.
  const groups = $derived(
    query.repository === ""
      ? groupByRepository(visible)
      : [{ repository: query.repository, label: repositoryLabel(query.repository), rows: visible }],
  );
  const showGroupHeadings = $derived(groups.length > 1);
  const counts = $derived(statusCounts(rows));
  let machineOptions = $derived.by((): SelectDropdownOption[] => {
    const names = new Set(rows.flatMap((row) => row.machineNames));
    if (query.hardware !== "") names.add(query.hardware);
    return [
      { value: "", label: "All machines" },
      ...[...names].sort().map((name) => ({ value: name, label: name })),
    ];
  });
  const windowOptions: { value: BrowseWindow; label: string }[] = [
    { value: "all", label: "All time" },
    { value: "30d", label: "30 days" },
    { value: "3mo", label: "3 months" },
    { value: "1y", label: "1 year" },
  ];
  const yAxisOptions: SelectDropdownOption[] = [
    { value: "zero", label: "Zero baseline" },
    { value: "observed", label: "Observed range" },
  ];

  function plural(n: number, word: string): string {
    return `${n.toLocaleString()} ${n === 1 ? word : `${word}s`}`;
  }
</script>

<main class="page series-page">
  <header class="page-header">
    <div>
      <p class="eyebrow">{query.repository === "" ? "All projects" : repositoryLabel(query.repository)}</p>
      <h1>Benchmarks</h1>
    </div>
    <div class="header-actions">
      {#if !loading && errorMsg === null}
        <div class="page-meta" aria-label="Benchmark summary">
          <span>{plural(rows.length, "benchmark")}{nextCursor !== null ? " loaded" : ""}</span>
          {#if counts.regressed > 0}<span class="meta-regressed">{counts.regressed} regressed</span>{/if}
          {#if counts.improved > 0}<span class="meta-improved">{counts.improved} improved</span>{/if}
        </div>
      {/if}
      <a class="button-pill secondary" href={appURL("/results")} onclick={(e) => go(e, "/results")}>Result explorer</a>
    </div>
  </header>

  <div class="panel browse-filters">
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
    <div class="machine-select">
      <SelectDropdown
        value={query.hardware}
        options={machineOptions}
        title="Machine"
        onchange={(hardware) => setFilter({ hardware })}
      />
    </div>
    <div class="segmented-control" role="group" aria-label="Time window">
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
    {#if hasFilters}
      <button type="button" class="button-pill secondary" onclick={() => navigate(clearedURL)}>Clear filters</button>
    {/if}
    <div class="view-controls">
      {#if chartView}
        <div class="y-axis-select">
          <SelectDropdown
            value={zeroBased ? "zero" : "observed"}
            options={yAxisOptions}
            title="Y-axis"
            onchange={(value) => (zeroBased = value === "zero")}
          />
        </div>
      {/if}
      <div class="segmented-control" role="group" aria-label="View">
        <button type="button" class:active={!chartView} aria-pressed={!chartView}
          onclick={() => setFilter({ view: "table" })}>Table</button>
        <button type="button" class:active={chartView} aria-pressed={chartView}
          onclick={() => setFilter({ view: "charts" })}>Charts</button>
      </div>
    </div>
  </div>

  {#if errorMsg}
    <section class="panel state-panel error-panel" role="alert">
      <h2>Failed to load benchmarks</h2>
      <p>{errorMsg}</p>
    </section>
  {:else if loading}
    <section class="panel state-panel loading-panel" aria-live="polite">
      <p>Loading benchmarks…</p>
    </section>
  {:else if visible.length === 0}
    <section class="panel state-panel empty-panel" aria-label="No matching benchmarks">
      <h2>{hasFilters ? "No benchmarks match these filters" : "No benchmarks yet"}</h2>
    </section>
  {:else}
    {#each groups as group (group.repository)}
      <section class="benchmark-group" aria-label={group.label}>
        {#if showGroupHeadings}
          <h2 class="group-heading">
            <a class="wrap-anywhere" href={appURL(`/series${formatBrowseQuery({ ...query, repository: group.repository, hardware: "" })}`)}
              onclick={(e) => go(e, `/series${formatBrowseQuery({ ...query, repository: group.repository, hardware: "" })}`)}
            >{group.label}</a>
            <span>{plural(group.rows.length, "benchmark")}</span>
          </h2>
        {/if}
        {#if chartView}
          <div class="trend-grid">
            {#each group.rows as row (row.benchmarkId)}
              <BrowseTrendCard {row} {baseUrl} {zeroBased} {timeRange} onopen={open} />
            {/each}
          </div>
        {:else}
          <BrowseTable rows={group.rows} {sort} onsort={toggleSort} onopen={open} />
        {/if}
      </section>
    {/each}
    {#if nextCursor !== null}
      <p class="scope-note">Sorting covers loaded benchmarks only.</p>
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
  .meta-regressed { color: var(--c-error); }
  .meta-improved { color: var(--c-success); }
  @media (max-width: 760px) {
    .header-actions { justify-items: start; }
  }
  .browse-filters {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 10px;
    padding: 10px 12px;
  }

  .benchmark-search { width: min(320px, 100%); }
  .machine-select { min-width: 180px; }
  .y-axis-select { min-width: 150px; }
  .machine-select :global(.kit-select-dropdown__trigger),
  .y-axis-select :global(.kit-select-dropdown__trigger) { width: 100%; }
  .view-controls {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-left: auto;
  }
  .benchmark-group {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 8px;
  }
  .group-heading {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 8px;
    margin: 6px 0 0;
    font-size: 0.95rem;
  }
  .group-heading a {
    min-width: 0;
    color: var(--c-text);
    text-decoration: none;
  }
  .group-heading a:hover { text-decoration: underline; }
  .group-heading span {
    color: var(--c-text-muted);
    font-size: 0.75rem;
    font-weight: 500;
  }
  .trend-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 420px), 1fr));
    gap: 10px;
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
    .view-controls { margin-left: 0; }
  }
</style>
