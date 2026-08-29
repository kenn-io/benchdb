<script lang="ts">
  import {
    DateRangePicker,
    localDateStr,
    type RangeSelection,
  } from "@kenn-io/kit-ui/date-range-picker";
  import {
    SelectDropdown,
    type SelectDropdownOption,
  } from "@kenn-io/kit-ui/select-dropdown";
  import { onMount, tick } from "svelte";

  import { createBenchDBClient } from "../api/client";
  import { formatMeasurement } from "../format";
  import { loadTrend, type TrendSource, type TrendViewModel } from "../series/loader";
  import {
    flagsText,
    type SeriesPoint,
    type TableRow,
    windowAnchorDate,
    windowPoints,
  } from "../series/transform";
  import {
    formatCompareQuery,
    formatTrendQuery,
    interceptNavClick,
    navigate,
    type TrendQuery,
    type TrendSigma,
  } from "../router";
  import { formatDate, tagsText } from "../browse/transform";
  import DetailTable from "./DetailTable.svelte";
  import EnvironmentDetails from "./EnvironmentDetails.svelte";
  import FleetSeriesChart from "./FleetSeriesChart.svelte";
  import MeasurementValue from "./MeasurementValue.svelte";
  import SeriesChart from "./SeriesChart.svelte";

  let {
    source,
    query,
    baseUrl = "",
  }: {
    source: TrendSource;
    query: TrendQuery;
    baseUrl?: string;
  } = $props();

  const TREND_TABLE_INITIAL_ROWS = 200;
  const TREND_TABLE_ROW_CHUNK = 200;
  const yAxisOptions: SelectDropdownOption[] = [
    { value: "zero", label: "Zero baseline" },
    { value: "observed", label: "Observed range" },
  ];
  type TrendFilter = "all" | "outliers" | "steps";
  type FlagTarget = {
    filter: Exclude<TrendFilter, "all">;
    label: string;
    count: number;
    point: SeriesPoint;
  };

  let vm = $state<TrendViewModel | null>(null);
  let errorMsg = $state<string | null>(null);
  let selectedResultId = $state<string | null>(null);
  let rowLimit = $state(TREND_TABLE_INITIAL_ROWS);
  let trendFilter = $state<TrendFilter>("all");
  let exportCopied = $state(false);
  let machineFilter = $state("all");
  let refreshing = $state(false);
  let refreshError = $state(false);
  let lastCheckedAt = $state<number | null>(null);
  let newPointCount = $state(0);
  let latestArrival = $state<{ machineName: string; point: SeriesPoint } | null>(null);

  function addedResults(
    previous: TrendViewModel,
    next: TrendViewModel,
  ): { machineName: string; point: SeriesPoint }[] {
    const previousIDs = new Set(
      previous.tracks.flatMap((track) => track.points.map((point) => point.resultId)),
    );
    return next.tracks.flatMap((track) =>
      track.points
        .filter((point) => !previousIDs.has(point.resultId))
        .map((point) => ({ machineName: track.machineName, point })),
    );
  }

  async function refreshTrend(initial = false) {
    if (refreshing) return;
    refreshing = true;
    try {
      const next = await loadTrend(createBenchDBClient(baseUrl), source);
      if (vm !== null) {
        const arrivals = addedResults(vm, next);
        newPointCount += arrivals.length;
        latestArrival = arrivals[arrivals.length - 1] ?? latestArrival;
      }
      vm = next;
      errorMsg = null;
      lastCheckedAt = Date.now();
      refreshError = false;
    } catch (err) {
      if (initial || vm === null) {
        errorMsg = err instanceof Error ? err.message : String(err);
      } else {
        refreshError = true;
      }
    } finally {
      refreshing = false;
    }
  }

  onMount(() => {
    void refreshTrend(true);
    const interval = window.setInterval(() => void refreshTrend(), 30_000);
    return () => window.clearInterval(interval);
  });

  let tracks = $derived(vm?.tracks ?? []);
  let fleetPoints = $derived(
    tracks.flatMap((track) => track.points).sort((a, b) => a.chartMs - b.chartMs),
  );
  let machineSummaries = $derived(
    tracks.map((track) => ({
      machineName: track.machineName,
      pointCount: track.points.length,
      latest: track.points[track.points.length - 1] ?? null,
    })),
  );
  let fleetCommitCount = $derived(new Set(fleetPoints.map((point) => point.commitHash)).size);
  let machineOptions = $derived.by((): SelectDropdownOption[] => [
    { value: "all", label: `All machines (${fleetPoints.length})`, triggerLabel: "All machines" },
    ...machineSummaries.map((summary) => ({
      value: summary.machineName,
      label: `${summary.machineName} (${summary.pointCount})`,
      triggerLabel: summary.machineName,
    })),
  ]);
  let activeTrack = $derived(
    machineFilter === "all"
      ? (tracks.length === 1 ? tracks[0]! : null)
      : (tracks.find((track) => track.machineName === machineFilter) ?? null),
  );
  let all = $derived(
    activeTrack === null
      ? tracks.flatMap((track) => track.points).sort((a, b) => a.chartMs - b.chartMs)
      : activeTrack.points,
  );
  let rangeAnchor = $derived(windowAnchorDate(all, new Date()));
  let earliestDate = $derived.by(() => {
    if (all.length === 0) return null;
    return localDateStr(new Date(Math.min(...all.map((point) => point.chartMs))));
  });
  let latestDate = $derived(localDateStr(rangeAnchor));
  let fleetCoverageText = $derived(
    fleetPoints.length === 0
      ? "no results"
      : `${formatDate(new Date(fleetPoints[0]!.chartMs).toISOString())} – ${formatDate(new Date(fleetPoints[fleetPoints.length - 1]!.chartMs).toISOString())}`,
  );
  let visible = $derived(windowPoints(all, query.range, rangeAnchor));
  let visibleCommitCount = $derived(new Set(visible.map((point) => point.commitHash)).size);
  let visibleTracks = $derived(
    tracks
      .filter((track) => machineFilter === "all" || track.machineName === machineFilter)
      .map((track) => ({ ...track, points: windowPoints(track.points, query.range, rangeAnchor) }))
      .filter((track) => track.points.length > 0),
  );
  let currentResultId = $derived(source.kind === "result" ? source.resultId : null);
  let outlierCount = $derived(visible.filter((p) => p.stats.isOutlier).length);
  let stepCount = $derived(visible.filter((p) => p.stats.isStep || p.stats.beginsChange).length);
  let flagTargets = $derived(flaggedPointTargets(visible));
  let rows = $derived(filteredTableRows(visibleTracks, trendFilter));
  let displayedRows = $derived(rows.slice(0, rowLimit));
  let hiddenRowCount = $derived(Math.max(0, rows.length - displayedRows.length));
  let selectedIndex = $derived.by(() => {
    if (selectedResultId === null) return null;
    const index = visible.findIndex((point) => point.resultId === selectedResultId);
    return index < 0 ? null : index;
  });
  let selected = $derived(selectedIndex === null ? null : (visible[selectedIndex] ?? null));
  let exportResultID = $derived(
    selected?.resultId ??
      (source.kind === "result" ? source.resultId : (visible[visible.length - 1]?.resultId ?? null)),
  );
  let exportServerURL = $derived(baseUrl !== "" ? baseUrl : browserOrigin());
  let exportCommand = $derived(
    exportResultID === null
      ? null
      : `benchdb history export ${exportResultID} --server ${exportServerURL} --output history.csv`,
  );

  // Compare picks are page-local workflow state: the shareable artifact is the
  // /compare URL the bar produces, not the picking session. Picks reference
  // result ids, so range/axis/sigma changes never invalidate them.
  let baselinePick = $state<{ id: string; sha: string } | null>(null);
  let contenderPick = $state<{ id: string; sha: string } | null>(null);

  let compareHref = $derived(
    baselinePick !== null && contenderPick !== null
      ? `/compare${formatCompareQuery({
          baseline: baselinePick.id,
          contender: contenderPick.id,
          threshold: null,
          thresholdZ: null,
        })}`
      : null,
  );

  function clearPicks() {
    baselinePick = null;
    contenderPick = null;
  }

  // Range and table-filter changes intentionally clear the selection because
  // they re-window the selectable row set. Live refreshes do not: the selected
  // result id is remapped to its new position when historical points arrive.
  $effect(() => {
    void query.range;
    void trendFilter;
    void machineFilter;
    selectedResultId = null;
    rowLimit = TREND_TABLE_INITIAL_ROWS;
    exportCopied = false;
  });

  $effect(() => {
    void exportCommand;
    exportCopied = false;
  });

  function basePath(): string {
    return source.kind === "benchmark"
      ? `/series/${source.benchmarkId}`
      : `/benchmarks/history/${source.resultId}`;
  }

  function setControl(patch: Partial<TrendQuery>) {
    navigate(`${basePath()}${formatTrendQuery({ ...query, ...patch })}`);
  }

  function setRange(range: RangeSelection) {
    setControl({ range });
  }

  function select(index: number) {
    selectedResultId = visible[index]?.resultId ?? null;
  }

  function selectRow(row: TableRow) {
    selectedResultId = row.resultId;
  }

  function openResult(resultId: string) {
    navigate(`/results/${resultId}`);
  }

  function browserOrigin(): string {
    if (typeof window === "undefined") return "";
    return window.location.origin;
  }

  function pointMatchesFilter(point: SeriesPoint, filter: TrendFilter): boolean {
    if (filter === "outliers") return point.stats.isOutlier;
    if (filter === "steps") return point.stats.isStep || point.stats.beginsChange;
    return true;
  }

  function filteredTableRows(sourceTracks: typeof visibleTracks, filter: TrendFilter): TableRow[] {
    return sourceTracks
      .flatMap((track) => track.points.map((point) => ({ point, machineName: track.machineName })))
      .sort((a, b) => a.point.chartMs - b.point.chartMs)
      .flatMap(({ point, machineName }, index) => {
        if (!pointMatchesFilter(point, filter)) return [];
        return [
          {
            index,
            resultId: point.resultId,
            commitHash: point.commitHash,
            commitMessage: point.commitMessage,
            chartMs: point.chartMs,
            svs: point.svs,
            unit: point.unit,
            z: point.stats.z,
            flags: flagsText(point.stats),
            machineName,
          },
        ];
      })
      .sort((a, b) => b.chartMs - a.chartMs || b.resultId.localeCompare(a.resultId));
  }

  function flaggedPointTargets(points: SeriesPoint[]): FlagTarget[] {
    const outliers = points.filter((point) => point.stats.isOutlier);
    const steps = points.filter((point) => point.stats.isStep || point.stats.beginsChange);
    return [
      targetFor("outliers", "outlier", outliers),
      targetFor("steps", "step", steps),
    ].filter((target): target is FlagTarget => target !== null);
  }

  function targetFor(
    filter: Exclude<TrendFilter, "all">,
    label: string,
    entries: SeriesPoint[],
  ): FlagTarget | null {
    const first = entries[0];
    if (first === undefined) return null;
    return { filter, label, count: entries.length, point: first };
  }

  function zText(value: number | null): string {
    return value === null ? "z —" : `z ${value.toFixed(2)}`;
  }

  function rowCountText(): string {
    if (trendFilter === "all") {
      return `showing ${displayedRows.length} of ${rows.length} points`;
    }
    return `showing ${displayedRows.length} of ${rows.length} filtered points`;
  }

  function checkedAtText(): string {
    if (lastCheckedAt === null) return "connecting";
    return `checked ${new Intl.DateTimeFormat(undefined, {
      hour: "numeric",
      minute: "2-digit",
      second: "2-digit",
    }).format(new Date(lastCheckedAt))}`;
  }

  function resultCountText(count: number): string {
    return `${count} ${count === 1 ? "result" : "results"}`;
  }

  async function copyExportCommand() {
    const command = exportCommand;
    if (command === null || navigator.clipboard === undefined) return;
    try {
      await navigator.clipboard.writeText(command);
      if (exportCommand === command) {
        exportCopied = true;
      }
    } catch {
      if (exportCommand === command) {
        exportCopied = false;
      }
    }
  }

  async function jumpToFlag(target: FlagTarget) {
    trendFilter = target.filter;
    rowLimit = TREND_TABLE_INITIAL_ROWS;
    await tick();
    selectedResultId = target.point.resultId;
  }

  function orientation(lessIsBetter: boolean | null): string | null {
    if (lessIsBetter === null) return null;
    return lessIsBetter ? "lower is better" : "higher is better";
  }
</script>

{#if errorMsg}
  <main class="page trend-page">
    <section class="panel state-panel">
      <h1>Trend unavailable</h1>
      <p class="error">Failed to load series: {errorMsg}</p>
      {#if source.kind === "result"}
        <a
          class="button-pill"
          href={`/results/${source.resultId}`}
          onclick={(e) => {
            if (!interceptNavClick(e)) return;
            e.preventDefault();
            openResult(source.resultId);
          }}
        >Open result details</a>
      {/if}
    </section>
  </main>
{:else if !vm}
  <main class="page trend-page">
    <section class="panel state-panel">
      <h1>Loading trend</h1>
      <p>Loading...</p>
    </section>
  </main>
{:else}
  <main class="page trend-page">
    <section class="trend-context panel" aria-label="Trend context">
      <header class="page-header">
        <div>
          <p class="eyebrow">Benchmark trend</p>
          <h1 title={vm.identity.benchmarkId}>{vm.identity.benchmarkName}</h1>
          <div class="ident page-subtitle">
            {#if tagsText(vm.identity.caseTags) !== ""}<span>{tagsText(vm.identity.caseTags)}</span>{/if}
            <span>{vm.tracks.length} {vm.tracks.length === 1 ? "machine" : "machines"}</span>
            <span title={vm.identity.repository}>{vm.identity.repositoryLabel}</span>
            {#if vm.identity.unit !== null}
              <span>
                unit: {vm.identity.unit}{orientation(vm.identity.lessIsBetter) !== null
                  ? ` (${orientation(vm.identity.lessIsBetter)})`
                  : ""}
              </span>
            {/if}
          </div>
        </div>
        <div class="live-status" class:warning={refreshError} aria-live="polite">
          <span class="live-dot"></span>
          <span>{refreshError ? "refresh failed" : newPointCount > 0 ? `${newPointCount} new ${newPointCount === 1 ? "result" : "results"}` : checkedAtText()}</span>
          {#if latestArrival !== null}
            <span class="arrival-detail">
              {latestArrival.machineName} · {latestArrival.point.commitHash.slice(0, 8)} ·
              {formatDate(new Date(latestArrival.point.chartMs).toISOString())}
            </span>
          {/if}
          <button type="button" class="refresh-button" disabled={refreshing} onclick={() => refreshTrend()}>
            {refreshing ? "Refreshing…" : "Refresh"}
          </button>
        </div>
      </header>
      {#if !vm.unitConsistent}
        <div class="integrity" role="alert">
          data integrity: this series mixes units ({vm.units.map((unit) => unit ?? "unit not set").join(", ")}) — values are
          not directly comparable
        </div>
      {/if}

    {#if all.length > 0}
    <div class="context-toolbar">
      <div class="toolbar controls">
        <label class="filter-label machine-select">
          machine
          <SelectDropdown
            value={machineFilter}
            options={machineOptions}
            title="Machine"
            onchange={(value) => (machineFilter = value)}
          />
        </label>
        <div class="filter-label range-control">
          <span>range</span>
          <DateRangePicker
            selection={query.range}
            onSelect={setRange}
            {earliestDate}
            maxDate={latestDate}
          />
        </div>
        <label class="filter-label">
          band
          <select
            value={String(query.sigma)}
            onchange={(e) => setControl({ sigma: Number(e.currentTarget.value) as TrendSigma })}
          >
            <option value="1">±1σ</option>
            <option value="2">±2σ</option>
            <option value="3">±3σ</option>
            <option value="5">±5σ</option>
          </select>
        </label>
        <label class="filter-label machine-select">
          Y-axis
          <SelectDropdown
            value={query.yAxis}
            options={yAxisOptions}
            title="Y-axis"
            onchange={(value) => setControl({ yAxis: value === "observed" ? "observed" : "zero" })}
          />
        </label>
      </div>

      <p class="summary-line context-summary" aria-label="Trend summary">
        <span class="summary-item">{visible.length} machine {visible.length === 1 ? "result" : "results"}</span>
        <span class="summary-item">{visibleCommitCount} {visibleCommitCount === 1 ? "commit" : "commits"}</span>
        <span
          class="summary-item"
          title="The x-axis uses commit time. Backfilled results appear at the commit's date."
        >{fleetCoverageText}</span>
        <span class="summary-item">{outlierCount} {outlierCount === 1 ? "outlier" : "outliers"}</span>
        <span class="summary-item">{stepCount} {stepCount === 1 ? "step" : "steps"}</span>
      </p>
    </div>

    {#if flagTargets.length > 0}
      <section class="flag-queue" aria-label="Flagged point shortcuts">
        {#each flagTargets as target}
          <button
            type="button"
            class="flag-card"
            aria-label={`Jump to first ${target.label}: ${target.point.commitHash}`}
            onclick={() => jumpToFlag(target)}
          >
            <span class="flag-count">{target.count} {target.count === 1 ? target.label : `${target.label}s`}</span>
            <strong title={target.point.commitHash}>{target.point.commitHash.slice(0, 12)}</strong>
            <span class="numeric-text">{formatMeasurement(target.point.svs, target.point.unit)} · {zText(target.point.stats.z)}</span>
            <span class="jump">View →</span>
          </button>
        {/each}
      </section>
    {/if}

    <!-- The bar lives outside the windowed branch: picks are id-based and
         survive range changes, so switching to an empty window must not
         strand a pending comparison. -->
    {#if baselinePick !== null || contenderPick !== null}
      <div class="compare-bar">
        <span>baseline: {baselinePick?.sha ?? "—"}</span>
        <span>contender: {contenderPick?.sha ?? "—"}</span>
        {#if compareHref !== null}
          {@const href = compareHref}
          <a
            class="button-pill primary"
            {href}
            onclick={(e) => {
              if (!interceptNavClick(e)) return;
              e.preventDefault();
              navigate(href);
            }}
          >Compare</a>
        {:else}
          <span class="faint">pick both points to compare</span>
        {/if}
        <button type="button" class="button-pill" onclick={clearPicks}>clear</button>
      </div>
    {/if}

    {/if}
  </section>

  {#if all.length === 0}
    <p class="empty">This series has no default-branch history.</p>
  {:else}
    {#if visible.length === 0}
      <p class="empty">
        No points in the selected range —
        <button
          type="button"
          class="link"
          onclick={() => setRange({ mode: "relative", days: 0 })}
        >
          show all {all.length} points
        </button>
      </p>
    {:else}
      {#if !vm.unitConsistent}
        <p class="empty chart-suppressed">Chart unavailable because this benchmark mixes measurement units.</p>
      {:else if machineFilter === "all" && visibleTracks.length > 1}
        <FleetSeriesChart
          tracks={visibleTracks}
          sigma={query.sigma}
          zeroBased={query.yAxis === "zero"}
          onopen={openResult}
        />
      {:else}
        <SeriesChart
          points={visible}
          sigma={query.sigma}
          zeroBased={query.yAxis === "zero"}
          {selectedIndex}
          {currentResultId}
          onselect={select}
          onopen={openResult}
        />
      {/if}
      {#if selected !== null}
        <!-- @const pins the narrowed point: TS narrowing on the nullable $derived
             does not survive into the onclick closure. -->
        {@const sel = selected}
        <section class="selected-panel panel" aria-label="Selected point">
          <div>
            <span class="eyebrow">selected point</span>
            <strong>{sel.commitHash}</strong>
            <span class="faint numeric-text"><MeasurementValue value={sel.svs} unit={sel.unit} /></span>
          </div>
          <dl class="point-meta">
            <div>
              <dt>result</dt>
              <dd>{sel.resultId}</dd>
            </div>
            <div>
              <dt>z-score</dt>
              <dd>{zText(sel.stats.z)}</dd>
            </div>
            <div>
              <dt>flags</dt>
              <dd>{flagsText(sel.stats) || "none"}</dd>
            </div>
          </dl>
          <div class="actions">
            <a
              class="button-pill"
              href={`/results/${sel.resultId}`}
              onclick={(e) => {
                if (!interceptNavClick(e)) return;
                e.preventDefault();
                openResult(sel.resultId);
              }}
            >Open result</a>
            <button
              type="button"
              class="button-pill"
              onclick={() => (baselinePick = { id: sel.resultId, sha: sel.commitHash })}
            >set baseline</button>
            <button
              type="button"
              class="button-pill"
              onclick={() => (contenderPick = { id: sel.resultId, sha: sel.commitHash })}
            >set contender</button>
          </div>
        </section>
      {/if}
      <section class="panel table-panel history-table-panel" aria-label="Trend history">
        <header class="history-heading">
          <div>
            <h2>Results</h2>
            <p class="row-count">{rowCountText()} · newest first</p>
          </div>
          <div class="filter-bar" aria-label="Trend point filters">
            <button
              type="button"
              class="button-pill"
              class:active={trendFilter === "all"}
              aria-pressed={trendFilter === "all"}
              onclick={() => (trendFilter = "all")}
            >All {visible.length}</button>
            <button
              type="button"
              class="button-pill"
              class:active={trendFilter === "outliers"}
              aria-pressed={trendFilter === "outliers"}
              onclick={() => (trendFilter = "outliers")}
            >Outliers {outlierCount}</button>
            <button
              type="button"
              class="button-pill"
              class:active={trendFilter === "steps"}
              aria-pressed={trendFilter === "steps"}
              onclick={() => (trendFilter = "steps")}
            >Steps {stepCount}</button>
          </div>
        </header>
        <DetailTable
          rows={displayedRows}
          {selectedResultId}
          onselect={selectRow}
          onopen={(row) => openResult(row.resultId)}
        />
      </section>
      {#if hiddenRowCount > 0}
        <button type="button" class="button-pill more" onclick={() => (rowLimit += TREND_TABLE_ROW_CHUNK)}>
          Show more
        </button>
      {/if}
      <section class="secondary-tools">
        {#if activeTrack !== null && activeTrack.segments.length > 0}
          <EnvironmentDetails context={activeTrack.segments[activeTrack.segments.length - 1]!.context} label="Selected machine environment" />
        {/if}
        {#if exportCommand !== null}
          <details class="export-panel panel" role="region" aria-label="History export">
            <summary>Export history</summary>
            <div class="export-content">
              <code>{exportCommand}</code>
              <button type="button" class="button-pill" onclick={copyExportCommand}>Copy export command</button>
              {#if exportCopied}<span class="copied">copied</span>{/if}
            </div>
          </details>
        {/if}
      </section>
    {/if}
  {/if}
  </main>
{/if}

<style>
  .trend-page {
    max-width: 1600px;
  }
  .trend-context {
    display: grid;
    gap: 8px;
    padding: 10px 12px;
    background: var(--c-surface);
  }
  .ident {
    display: flex;
    gap: 10px;
    flex-wrap: wrap;
    overflow-wrap: anywhere;
  }
  .faint { color: var(--c-text-faint); }
  .integrity {
    padding: 7px 9px;
    border-radius: var(--radius-sm);
    background: var(--c-warn-bg);
    border: 1px solid var(--c-warning);
    color: var(--c-warn-text);
    font-size: 0.8rem;
  }
  .error { color: var(--c-error); }
  .empty { color: var(--c-text-muted); }
  .controls {
    align-items: end;
  }
  .controls .filter-label {
    min-width: 120px;
  }
  .context-toolbar {
    display: flex;
    align-items: flex-end;
    justify-content: space-between;
    gap: 8px 20px;
    flex-wrap: wrap;
  }
  .context-summary {
    justify-content: flex-end;
    padding-bottom: 4px;
  }
  .live-status {
    display: flex;
    align-items: center;
    gap: 7px;
    color: var(--c-text-muted);
    font-size: 0.76rem;
    white-space: nowrap;
  }
  .live-dot { width: 7px; height: 7px; border-radius: 50%; background: var(--c-success); }
  .live-status.warning .live-dot { background: var(--c-error); }
  .arrival-detail {
    color: var(--c-text);
    font-variant-numeric: tabular-nums;
  }
  .refresh-button {
    padding: 3px 7px;
    border: 1px solid var(--c-border-muted);
    border-radius: var(--radius-sm);
    background: var(--c-surface);
    color: var(--c-text);
    cursor: pointer;
  }
  .refresh-button:disabled { cursor: wait; opacity: 0.65; }
  .flag-queue {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    margin: 0;
  }
  .flag-card {
    display: flex;
    align-items: center;
    flex: 1 1 360px;
    gap: 8px;
    min-width: 0;
    min-height: 34px;
    padding: 0.3rem 0.55rem;
    border: 1px solid var(--c-border-muted);
    border-left: 3px solid var(--c-warning);
    border-radius: var(--radius-sm);
    background: var(--c-surface);
    color: var(--c-text-muted);
    cursor: pointer;
    font: inherit;
    font-size: 0.78rem;
    text-align: left;
  }
  .flag-card:hover {
    border-color: var(--c-accent);
  }
  .flag-card strong {
    color: var(--c-text);
    font-variant-numeric: tabular-nums;
    overflow-wrap: anywhere;
  }
  .flag-count {
    color: var(--c-warning);
    font-weight: 700;
    white-space: nowrap;
  }
  .flag-card .jump {
    margin-left: auto;
    color: var(--c-accent);
    font-weight: 700;
    white-space: nowrap;
  }
  .filter-bar { display: flex; gap: 0.45rem; flex-wrap: wrap; }
  .selected-panel {
    margin: 0.65rem 0;
    padding: 0.65rem 0.75rem;
  }
  .selected-panel {
    display: grid;
    grid-template-columns: minmax(10rem, 1.2fr) minmax(12rem, 2fr) auto;
    gap: 0.9rem;
    align-items: center;
  }
  .selected-panel strong, .selected-panel .faint { display: block; overflow-wrap: anywhere; }
  .eyebrow {
    display: block;
    color: var(--c-text-muted);
    font-size: 0.68rem;
    text-transform: uppercase;
    letter-spacing: 0;
    margin-bottom: 0.15rem;
  }
  .point-meta {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.6rem;
    margin: 0;
  }
  .point-meta div { min-width: 0; }
  .point-meta dt {
    color: var(--c-text-muted);
    font-size: 0.68rem;
    text-transform: uppercase;
    letter-spacing: 0;
  }
  .point-meta dd { margin: 0.08rem 0 0; overflow-wrap: anywhere; font-variant-numeric: tabular-nums; }
  .actions { display: flex; gap: 0.65rem; align-items: center; flex-wrap: wrap; justify-content: flex-end; }
  .secondary-tools { display: grid; gap: 8px; }
  .export-panel { padding: 0.65rem 0.75rem; }
  .export-panel summary { cursor: pointer; color: var(--c-text-muted); font-weight: 650; }
  .export-content { display: flex; gap: 0.55rem; align-items: center; flex-wrap: wrap; margin-top: 0.6rem; }
  .export-panel code {
    overflow-wrap: anywhere;
    font-size: 0.78rem;
    padding: 0.16rem 0.28rem;
    background: var(--c-bg-inset);
    border-radius: 4px;
  }
  .copied { color: var(--c-success); font-size: 0.78rem; }
  .compare-bar {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
    padding: 8px;
    border: 1px solid var(--c-border-muted);
    border-radius: var(--radius-sm);
    background: var(--c-bg-inset);
    font-size: 0.8rem;
    color: var(--c-text-muted);
  }
  .history-table-panel {
    overflow: hidden;
  }
  .history-heading { display: flex; justify-content: space-between; align-items: center; gap: 12px; padding: 10px; border-bottom: 1px solid var(--c-border-muted); }
  .history-heading h2 { margin: 0; font-size: 0.95rem; }
  .row-count { color: var(--c-text-muted); font-size: 0.76rem; margin: 2px 0 0; }
  .more {
    margin-top: 0.6rem;
  }
  .link { background: none; border: none; padding: 0; font: inherit;
          color: var(--c-accent); cursor: pointer; text-decoration: underline; }
  @media (max-width: 760px) {
    .page-header {
      display: grid;
    }
    .live-status { flex-wrap: wrap; white-space: normal; }
    .history-heading { align-items: flex-start; flex-direction: column; }
    .context-summary { justify-content: flex-start; padding-bottom: 0; }
    .flag-card { align-items: flex-start; flex-wrap: wrap; }
    .flag-card .jump { margin-left: 0; }
    .selected-panel { grid-template-columns: 1fr; align-items: start; }
    .point-meta { grid-template-columns: 1fr; }
    .actions { justify-content: flex-start; }
  }
</style>
