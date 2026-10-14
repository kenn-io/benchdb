<script lang="ts">
  import { appURL } from "../base-path";
  import { SearchInput } from "@kenn-io/kit-ui/search-input";
  import { SelectDropdown, type SelectDropdownOption } from "@kenn-io/kit-ui/select-dropdown";
  import { onDestroy } from "svelte";

  import { createBenchDBClient } from "../api/client";
  import { listSeries, type BrowsePage, type BrowseStatusCounts } from "../browse/loader";
  import {
    groupByRepository,
    mergeRows,
    sortRows,
    windowStartIso,
    type BrowseRow,
    type SeriesStatus,
    type SortKey,
    type SortSpec,
  } from "../browse/transform";
  import { repositoryLabel } from "../repository";
  import {
    DEFAULT_BROWSE_QUERY,
    formatBrowseQuery,
    parseBrowseQuery,
    interceptNavClick,
    navigate,
    type BrowseQuery,
    type BrowseSort,
    type BrowseWindow,
  } from "../router";
  import BrowseTable from "./BrowseTable.svelte";
  import BrowseTrendCard from "./BrowseTrendCard.svelte";
  import SubmitResultsHint from "./SubmitResultsHint.svelte";

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
  let statusCounts = $state<BrowseStatusCounts | null>(null);
  let statusesPending = $state(false);
  let loading = $state(true);
  let loadingMore = $state(false);
  let errorMsg = $state<string | null>(null);
  let moreErrorMsg = $state<string | null>(null);
  let refreshError = $state<string | null>(null);
  // Sort and y-axis live in the URL, so a shared link keeps them.
  const sort = $derived.by((): SortSpec | null => {
    if (query.sort === "") return null;
    const [key, dir] = query.sort.split("-");
    return { key: key as SortKey, dir: dir === "desc" ? "desc" : "asc" };
  });
  const zeroBased = $derived(query.yAxis === "zero");
  // Every machine with history in the selected project, not just those on the
  // loaded pages.
  let machines = $state<{ repository: string; names: string[] } | null>(null);
  let searchFilter = $state("");
  const chartView = $derived(query.view === "charts");
  // Clearing filters keeps the project and the view preferences.
  const clearedURL = $derived(
    `/series${formatBrowseQuery({
      ...DEFAULT_BROWSE_QUERY, repository: query.repository, view: query.view, sort: query.sort, yAxis: query.yAxis,
    })}`,
  );
  const hasFilters = $derived(
    query.q !== "" || query.hardware !== "" || query.window !== "all" || query.status !== "",
  );
  // View-only navigation keeps the loaded rows and pagination cursor.
  const filterQuery = $derived(formatBrowseQuery({ ...query, view: "table", sort: "", yAxis: "observed" }));

  // The machine list reloads after each first-page load or refresh, so it
  // follows the same scoring the rows do, and is kept with its project so
  // another project's list is never shown. A failed reload keeps the last list.
  let machinesToken = 0;
  function loadMachines(repository: string) {
    const token = ++machinesToken;
    void client.listMachines(repository === "" ? {} : { repository }).then((res) => {
      if (token === machinesToken && res.status === 200) machines = { repository, names: res.data.machines ?? [] };
    }).catch(() => {
      // The loaded rows' machines still fill the filter.
    });
  }
  const timeRange = $derived.by(() => {
    const now = new Date();
    const since = windowStartIso(query.window, now);
    return since === null ? null : { min: Date.parse(since), max: now.getTime() };
  });
  let searchTimer: ReturnType<typeof setTimeout> | undefined;
  // Monotonic token: a stale response (filters changed mid-flight) must not
  // overwrite a newer page.
  let reqToken = 0;
  // The server scores a benchmark within seconds of a new result. While a
  // score is queued, the first page reloads with a growing delay for up to a
  // minute; once more pages are loaded it stops, so rows never jump away.
  const REFRESH_FIRST_MS = 2000;
  const REFRESH_MAX_MS = 15_000;
  const REFRESH_BUDGET_MS = 60_000;
  let refreshDelay = REFRESH_FIRST_MS;
  let refreshWaited = 0;
  let refreshTimer: ReturnType<typeof setTimeout> | undefined;
  let morePagesLoaded = false;

  $effect(() => {
    const filters = parseBrowseQuery(filterQuery);
    searchFilter = filters.q;
    void load(filters);
  });

  function showPage(page: BrowsePage) {
    rows = page.rows;
    nextCursor = page.nextCursor;
    statusCounts = page.statusCounts;
    statusesPending = page.statusesPending;
  }

  async function load(q: BrowseQuery) {
    const token = ++reqToken;
    clearTimeout(refreshTimer);
    refreshDelay = REFRESH_FIRST_MS;
    refreshWaited = 0;
    morePagesLoaded = false;
    loading = true;
    rows = [];
    nextCursor = null;
    statusCounts = null;
    statusesPending = false;
    // A fresh page load supersedes any in-flight load-more, whose guarded
    // finally will then skip its cleanup — clear the flag here so the new
    // page's Load more cannot start out wedged disabled.
    loadingMore = false;
    errorMsg = null;
    moreErrorMsg = null;
    refreshError = null;
    try {
      const page = await listSeries(client, q);
      if (token !== reqToken) return;
      showPage(page);
      loadMachines(q.repository);
      scheduleRefresh(q);
    } catch (err) {
      if (token !== reqToken) return;
      errorMsg = err instanceof Error ? err.message : String(err);
    } finally {
      if (token === reqToken) loading = false;
    }
  }

  function scheduleRefresh(q: BrowseQuery) {
    clearTimeout(refreshTimer);
    if (!statusesPending || morePagesLoaded || refreshWaited >= REFRESH_BUDGET_MS) return;
    const delay = refreshDelay;
    refreshWaited += delay;
    refreshDelay = Math.min(Math.round(delay * 1.5), REFRESH_MAX_MS);
    refreshTimer = setTimeout(() => void refresh(q), delay);
  }

  async function refresh(q: BrowseQuery) {
    const token = reqToken;
    try {
      const page = await listSeries(client, q);
      if (token !== reqToken || morePagesLoaded) return;
      showPage(page);
      loadMachines(q.repository);
      refreshError = null;
    } catch (err) {
      if (token !== reqToken) return;
      // A failed refresh keeps the benchmarks already shown.
      refreshError = err instanceof Error ? err.message : String(err);
    }
    scheduleRefresh(q);
  }

  async function loadMore() {
    if (nextCursor === null || loadingMore) return;
    const token = reqToken;
    const hadMorePages = morePagesLoaded;
    loadingMore = true;
    moreErrorMsg = null;
    morePagesLoaded = true;
    clearTimeout(refreshTimer);
    try {
      const page = await listSeries(client, query, nextCursor);
      if (token !== reqToken) return;
      rows = mergeRows(rows, page.rows);
      nextCursor = page.nextCursor;
    } catch (err) {
      if (token !== reqToken) return;
      moreErrorMsg = err instanceof Error ? err.message : String(err);
      // Only the first page is shown, so it keeps refreshing.
      morePagesLoaded = hadMorePages;
      scheduleRefresh(parseBrowseQuery(filterQuery));
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
    clearTimeout(searchTimer);
    clearTimeout(refreshTimer);
  });

  function toggleSort(key: SortKey) {
    if (sort?.key !== key) {
      setFilter({ sort: `${key}-asc` });
    } else if (sort.dir === "asc") {
      setFilter({ sort: `${key}-desc` });
    } else {
      setFilter({ sort: "" });
    }
  }

  // Narrow screens hide the sortable column headers, so the same sorts are
  // offered as one select.
  const sortOptions: SelectDropdownOption[] = [
    { value: "", label: "Status, regressions first" },
    { value: "name-asc", label: "Name, A to Z" },
    { value: "name-desc", label: "Name, Z to A" },
    { value: "svs-asc", label: "Last value, low to high" },
    { value: "svs-desc", label: "Last value, high to low" },
    { value: "points-desc", label: "Most history" },
    { value: "points-asc", label: "Least history" },
    { value: "commit-desc", label: "Newest commit" },
    { value: "commit-asc", label: "Oldest commit" },
  ];

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
  const statusFilters: { status: SeriesStatus; label: string }[] = [
    { status: "regressed", label: "Regressed" },
    { status: "improved", label: "Improved" },
    { status: "stable", label: "Stable" },
    { status: "insufficient", label: "Insufficient" },
  ];
  const totalBenchmarks = $derived(
    statusCounts === null ? 0 : statusFilters.reduce((sum, { status }) => sum + statusCounts![status], 0),
  );
  const activeStatusLabel = $derived(statusFilters.find((option) => option.status === query.status)?.label ?? "");
  const emptyHeading = $derived(
    activeStatusLabel !== ""
      ? `No ${activeStatusLabel.toLowerCase()} benchmarks`
      : hasFilters
        ? "No benchmarks match these filters"
        : "No benchmarks yet",
  );

  function statusHref(status: BrowseQuery["status"]): string {
    return `/series${formatBrowseQuery({ ...query, status })}`;
  }
  let machineOptions = $derived.by((): SelectDropdownOption[] => {
    // Loaded rows can name a machine the last machine list predates.
    const listed = machines?.repository === query.repository ? machines.names : [];
    const names = new Set([...listed, ...rows.flatMap((row) => row.machineNames)]);
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
    { value: "zero", label: "Start at zero" },
    { value: "observed", label: "Fit to data" },
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
    <a class="button-pill secondary" href={appURL("/results")} onclick={(e) => go(e, "/results")}>All results</a>
  </header>

  {#if statusCounts !== null && (totalBenchmarks > 0 || query.status !== "")}
    <nav class="status-filters" aria-label="Filter benchmarks by status">
      <a class="button-pill" class:active={query.status === ""} aria-current={query.status === "" ? "true" : undefined}
        href={appURL(statusHref(""))} onclick={(e) => go(e, statusHref(""))}>
        All <strong class="filter-count">{totalBenchmarks.toLocaleString()}</strong>
      </a>
      {#each statusFilters as option (option.status)}
        {#if statusCounts[option.status] > 0 || query.status === option.status}
          <a class="button-pill status-filter {option.status}" class:active={query.status === option.status}
            aria-current={query.status === option.status ? "true" : undefined}
            href={appURL(statusHref(option.status))} onclick={(e) => go(e, statusHref(option.status))}>
            <span class="dot" aria-hidden="true"></span>
            {option.label} <strong class="filter-count">{statusCounts[option.status].toLocaleString()}</strong>
          </a>
        {/if}
      {/each}
      {#if statusesPending}<span class="pending-note" role="status">Updating statuses…</span>{/if}
    </nav>
    <p class="status-help">
      A status compares each machine's latest two results with its history.
      <a href="https://benchdb.io/docs/regression-analysis/" target="_blank" rel="noreferrer">How statuses work</a>
    </p>
  {/if}

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
      {#if !chartView}
        <div class="sort-select">
          <SelectDropdown
            value={query.sort}
            options={sortOptions}
            title="Sort"
            onchange={(value) => setFilter({ sort: value as BrowseSort })}
          />
        </div>
      {/if}
      {#if chartView}
        <div class="y-axis-select">
          <SelectDropdown
            value={query.yAxis}
            options={yAxisOptions}
            title="Y-axis"
            onchange={(value) => setFilter({ yAxis: value === "zero" ? "zero" : "observed" })}
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
      <button type="button" class="button-pill" onclick={() => void load(parseBrowseQuery(filterQuery))}>Retry</button>
    </section>
  {:else if loading}
    <section class="panel state-panel loading-panel" aria-live="polite">
      <p>Loading benchmarks…</p>
    </section>
  {:else if visible.length === 0}
    <section class="panel state-panel empty-panel" aria-label="No matching benchmarks">
      <h2>{emptyHeading}</h2>
      {#if statusesPending}<p>New results are still being scored. This page updates when they finish.</p>{/if}
      {#if !statusesPending && !hasFilters && activeStatusLabel === "" && query.repository === ""}<SubmitResultsHint />{/if}
    </section>
  {:else}
    {#if refreshError}
      <p class="refresh-error" role="status">Couldn't refresh benchmark statuses: {refreshError}</p>
    {/if}
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
    {#if sort !== null && nextCursor !== null}
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
  .status-filters {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
  }
  .status-filters .button-pill {
    gap: 6px;
  }
  .status-filter .dot {
    width: 0.45rem;
    height: 0.45rem;
    border-radius: 999px;
    background: var(--c-unknown);
  }
  .status-filter.regressed .dot { background: var(--c-error); }
  .status-filter.improved .dot { background: var(--c-success); }
  .status-filter.stable .dot { background: var(--c-stable); }
  .status-filter.insufficient .dot { background: var(--c-insufficient); }
  .filter-count {
    font-variant-numeric: tabular-nums;
  }
  .status-help {
    margin: 0;
    color: var(--c-text-muted);
    font-size: 0.78rem;
  }
  .pending-note,
  .refresh-error {
    margin: 0;
    color: var(--c-text-muted);
    font-size: 0.78rem;
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
  .sort-select { display: none; min-width: 190px; }
  .sort-select :global(.kit-select-dropdown__trigger) { width: 100%; }
  /* Column headers sort the full table; the select stands in once they hide. */
  @media (max-width: 1120px) {
    .sort-select { display: block; }
  }
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
