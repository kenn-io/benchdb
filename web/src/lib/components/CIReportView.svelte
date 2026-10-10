<script lang="ts">
  import { appURL } from "../base-path";
  import type { Snippet } from "svelte";

  import { createBenchDBClient } from "../api/client";
  import { toleranceText } from "../compare/transform";
  import { formatMeasurement } from "../format";
  import { loadCIReport, type CIReport } from "../ci-report/loader";
  import { interceptNavClick, navigate, type CIReportQuery } from "../router";

  type ReportRun = NonNullable<CIReport["runs"]>[number];
  type ReportComparison = NonNullable<ReportRun["comparisons"]>[number];
  type RowStatus = ReportComparison["status"];
  type StatusFilter = "all" | RowStatus;

  interface FilteredRun {
    run: ReportRun;
    comparisons: ReportComparison[];
  }

  interface MachineCoverage {
    key: string;
    name: string;
    total: number;
    compared: number;
    missingBaseline: number;
    benchmarkErrors: number;
    notComparable: number;
  }

  const CI_REPORT_INITIAL_ROWS = 200;
  const CI_REPORT_ROW_CHUNK = 200;

  const STATUS_FILTERS: RowStatus[] = [
    "regressed",
    "errored",
    "missing_baseline",
    "not_comparable",
    "improved",
    "stable",
    "insufficient",
  ];
  // Rows that need a decision come first; stable rows collapse by default.
  const STATUS_ORDER: Record<RowStatus, number> = {
    regressed: 0,
    errored: 1,
    not_comparable: 2,
    missing_baseline: 3,
    improved: 4,
    insufficient: 5,
    stable: 6,
  };
  const STATUS_LABELS: Record<RowStatus, string> = {
    regressed: "regressed",
    improved: "improved",
    stable: "stable",
    insufficient: "insufficient",
    errored: "errored",
    missing_baseline: "missing baseline",
    not_comparable: "not comparable",
  };

  let {
    query,
    baseUrl = "",
    header,
  }: {
    query: CIReportQuery;
    baseUrl?: string;
    header?: Snippet<[CIReport]>;
  } = $props();

  const client = $derived(createBenchDBClient(baseUrl));

  let report = $state<CIReport | null>(null);
  let errorMsg = $state<string | null>(null);
  let rowLimits = $state<Record<string, number>>({});
  let stableShown = $state<Record<string, boolean>>({});
  let statusFilter = $state<StatusFilter>("all");
  let hardwareFilter = $state("all");
  let searchText = $state("");
  let reqToken = 0;

  let runs = $derived(reportRuns(report));
  let allComparisons = $derived(runs.flatMap((run) => runComparisons(run)));
  let statusCounts = $derived(countStatuses(allComparisons));
  let hardwareOptions = $derived(
    Array.from(new Set(allComparisons.map((row) => row.hardware.name).filter((name) => name !== ""))).sort((a, b) =>
      a.localeCompare(b),
    ),
  );
  let filteredRuns = $derived(filteredReportRuns(runs));
  let filteredComparisons = $derived(filteredRuns.flatMap((entry) => entry.comparisons));
  let detailedComparisonCount = $derived(
    filteredRuns.reduce((sum, entry) => sum + detailedComparisons(entry.comparisons).length, 0),
  );
  let filteredStatusCounts = $derived(countStatuses(filteredComparisons));
  let machineCoverage = $derived(coverageByMachine(allComparisons));

  $effect(() => {
    const snapshot = { ...query };
    const token = ++reqToken;
    loadCIReport(client, snapshot)
      .then((loaded) => {
        if (token !== reqToken) return;
        report = loaded;
        rowLimits = {};
        stableShown = {};
        errorMsg = null;
      })
      .catch((err: unknown) => {
        if (token !== reqToken) return;
        errorMsg = err instanceof Error ? err.message : String(err);
      });
  });

  function go(e: MouseEvent, href: string) {
    if (!interceptNavClick(e)) return;
    e.preventDefault();
    navigate(href);
  }

  function reportRuns(value: CIReport | null): ReportRun[] {
    return value?.runs ?? [];
  }

  function runComparisons(run: ReportRun): ReportComparison[] {
    return run.comparisons ?? [];
  }

  function emptyStatusCounts(): Record<RowStatus, number> {
    return {
      regressed: 0,
      improved: 0,
      stable: 0,
      insufficient: 0,
      errored: 0,
      missing_baseline: 0,
      not_comparable: 0,
    };
  }

  function countStatuses(rows: ReportComparison[]): Record<RowStatus, number> {
    const counts = emptyStatusCounts();
    for (const row of rows) {
      counts[row.status] += 1;
    }
    return counts;
  }

  function coverageByMachine(rows: ReportComparison[]): MachineCoverage[] {
    const groups = new Map<string, MachineCoverage>();
    for (const row of rows) {
      const key = row.hardware.hash || row.hardware.id || row.hardware.name || "unknown-machine";
      const current = groups.get(key) ?? {
        key,
        name: row.hardware.name || "Unknown machine",
        total: 0,
        compared: 0,
        missingBaseline: 0,
        benchmarkErrors: 0,
        notComparable: 0,
      };
      current.total += 1;
      switch (row.status) {
        case "regressed":
        case "improved":
        case "stable":
        case "insufficient":
          current.compared += 1;
          break;
        case "missing_baseline":
          current.missingBaseline += 1;
          break;
        case "errored":
          current.benchmarkErrors += 1;
          break;
        case "not_comparable":
          current.notComparable += 1;
          break;
      }
      groups.set(key, current);
    }
    return Array.from(groups.values()).sort((a, b) => a.name.localeCompare(b.name));
  }

  function detailedComparisons(rows: ReportComparison[]): ReportComparison[] {
    if (statusFilter === "missing_baseline") {
      return rows;
    }
    return rows.filter((row) => row.status !== "missing_baseline");
  }

  function compareNames(a: string, b: string): number {
    return a.localeCompare(b, undefined, { numeric: true });
  }

  function stableCollapsed(runID: string): boolean {
    return statusFilter === "all" && searchText.trim() === "" && !stableShown[runID];
  }

  function shownComparisons(runID: string, rows: ReportComparison[]): ReportComparison[] {
    return stableCollapsed(runID) ? rows.filter((row) => row.status !== "stable") : rows;
  }

  function showStable(runID: string) {
    stableShown = { ...stableShown, [runID]: true };
  }

  function missingBaselineComparisons(rows: ReportComparison[]): ReportComparison[] {
    return rows.filter((row) => row.status === "missing_baseline");
  }

  function missingBaselineReason(rows: ReportComparison[]): string {
    const reasons = Array.from(new Set(rows.map((row) => row.reason?.trim() ?? "").filter(Boolean)));
    if (reasons.length === 1) return reasons[0] ?? "";
    if (reasons.length > 1) return "The selected baseline has multiple coverage gaps.";
    return "The selected baseline does not contain matching benchmark results.";
  }

  function filteredReportRuns(sourceRuns: ReportRun[]): FilteredRun[] {
    return sourceRuns
      .map((run) => ({
        run,
        comparisons: runComparisons(run)
          .filter((row) => matchesFilters(run, row))
          .sort((a, b) => STATUS_ORDER[a.status] - STATUS_ORDER[b.status] || compareNames(a.name, b.name)),
      }))
      .filter((entry) => entry.comparisons.length > 0 || entry.run.baseline_error !== null);
  }

  function matchesFilters(run: ReportRun, row: ReportComparison): boolean {
    if (statusFilter !== "all" && row.status !== statusFilter) {
      return false;
    }
    if (hardwareFilter !== "all" && row.hardware.name !== hardwareFilter) {
      return false;
    }
    const q = searchText.trim().toLowerCase();
    if (q === "") {
      return true;
    }
    return [
      run.run_id,
      run.run_reason ?? "",
      row.name,
      row.history_fingerprint,
      row.hardware.name,
      row.status,
      statusLabel(row.status),
      row.unit ?? "",
      row.reason ?? "",
    ].some((value) => value.toLowerCase().includes(q));
  }

  function setStatusFilter(status: StatusFilter) {
    statusFilter = status;
    rowLimits = {};
  }

  function setHardwareFilter(value: string) {
    hardwareFilter = value;
    rowLimits = {};
  }

  function setSearchText(value: string) {
    searchText = value;
    rowLimits = {};
  }


  function signedMeasurement(value: number, unit: string | null): string {
    return `${value > 0 ? "+" : ""}${formatMeasurement(value, unit)}`;
  }

  function valueText(value: number | null, unit: string | null): string {
    return value === null ? "-" : formatMeasurement(value, unit);
  }

  function baselineCommitText(run: ReportRun): string | null {
    const commit = run.baseline_commit;
    if (commit === null) return null;
    const when = commit.timestamp ? ` · ${new Date(commit.timestamp).toLocaleDateString()}` : "";
    return `${commit.sha.slice(0, 8)}${when}`;
  }

  // The API orients percent_change so positive means better. Show the raw
  // change instead, so its sign matches the values and the absolute delta, and
  // name the direction in words.
  function percentText(row: ReportComparison): string {
    const pairwise = row.analysis?.pairwise ?? null;
    if (pairwise === null) {
      return "-";
    }
    const raw = row.less_is_better === true ? -pairwise.percent_change : pairwise.percent_change;
    const rounded = Number(raw.toFixed(1));
    const display = Object.is(rounded, -0) ? 0 : rounded;
    const text = `${display > 0 ? "+" : ""}${display.toFixed(1)}%`;
    if (display === 0 || row.less_is_better === null) return text;
    return `${text} ${pairwise.percent_change > 0 ? "better" : "worse"}`;
  }

  function zText(row: ReportComparison): string {
    const lookback = row.analysis?.lookback_z_score ?? null;
    return lookback === null ? "-" : lookback.z_score.toFixed(2);
  }

  function reportStatusLabel(value: string): string {
    return value.replaceAll("_", " ");
  }

  function statusLabel(status: RowStatus): string {
    return STATUS_LABELS[status];
  }

  function rowLimit(runID: string): number {
    if (!Object.prototype.hasOwnProperty.call(rowLimits, runID)) {
      return CI_REPORT_INITIAL_ROWS;
    }
    return rowLimits[runID] ?? CI_REPORT_INITIAL_ROWS;
  }

  function showMore(runID: string) {
    rowLimits = { ...rowLimits, [runID]: rowLimit(runID) + CI_REPORT_ROW_CHUNK };
  }

  function filtersActive(): boolean {
    return statusFilter !== "all" || hardwareFilter !== "all" || searchText.trim() !== "";
  }

  function rowCountText(visible: number, filtered: number, total: number): string {
    if (visible === filtered && !filtersActive()) return "";
    const matching = filtersActive() ? " matching" : "";
    const suffix = filtered !== total ? ` (filtered from ${total.toLocaleString()})` : "";
    return `showing ${visible.toLocaleString()} of ${filtered.toLocaleString()}${matching} comparisons${suffix}`;
  }

  function plural(n: number, word: string, pluralWord = `${word}s`): string {
    return `${n.toLocaleString()} ${n === 1 ? word : pluralWord}`;
  }

</script>

{#if errorMsg}
  <p class="error" role="alert">Failed to load CI report: {errorMsg}</p>
{:else if !report}
  <p aria-live="polite">Loading report…</p>
{:else}
  {@const r = report}
  <div class="ci-report-view">
    {@render header?.(r)}

    <p class="summary-line" aria-label="CI report summary">
      <span class={`report-status ${r.status}`}>{reportStatusLabel(r.status)}</span>
      <span class="summary-item">{r.status_reason}</span>
      {#if r.summary.runs > 1}<span class="summary-item">{plural(r.summary.runs, "run")}</span>{/if}
      <strong class="summary-item" class:alert={r.summary.compared < r.summary.contender_results}>
        {r.summary.compared.toLocaleString()} of {r.summary.contender_results.toLocaleString()} compared
      </strong>
      <span class="summary-item" class:alert={r.summary.regressions > 0}>{plural(r.summary.regressions, "regression")}</span>
    </p>

    {#if r.summary.compared < r.summary.contender_results}
      <section class="panel coverage-panel" aria-label="Comparison coverage">
        <header class="coverage-head">
          <h2>Coverage</h2>
          <span class="coverage-warning">
            {plural(r.summary.contender_results - r.summary.compared, "result")} not compared
          </span>
        </header>
        <div class="coverage-grid">
          {#each machineCoverage as coverage (coverage.key)}
            <article class="coverage-card">
              <div class="coverage-card-head">
                <strong>{coverage.name}</strong>
                <span>{coverage.compared.toLocaleString()} / {coverage.total.toLocaleString()}</span>
              </div>
              <progress
                max={coverage.total}
                value={coverage.compared}
                aria-label={`${coverage.name}: ${coverage.compared} of ${coverage.total} results compared`}
              ></progress>
              <p>
                {#if coverage.missingBaseline > 0}<span>{coverage.missingBaseline} missing baseline</span>{/if}
                {#if coverage.benchmarkErrors > 0}<span>{coverage.benchmarkErrors} benchmark {coverage.benchmarkErrors === 1 ? "error" : "errors"}</span>{/if}
                {#if coverage.notComparable > 0}<span>{coverage.notComparable} not comparable</span>{/if}
                {#if coverage.compared === coverage.total}<span>complete</span>{/if}
              </p>
            </article>
          {/each}
        </div>
      </section>
    {/if}

    <section class="panel controls-panel" aria-label="CI report controls">
      <div class="status-tabs" aria-label="Filter comparisons by status">
        <button type="button" aria-pressed={statusFilter === "all"} onclick={() => setStatusFilter("all")}>
          <span>all</span>
          <strong>{allComparisons.length.toLocaleString()}</strong>
        </button>
        {#each STATUS_FILTERS.filter((status) => statusCounts[status] > 0 || statusFilter === status) as status}
          <button
            type="button"
            aria-pressed={statusFilter === status}
            onclick={() => setStatusFilter(status)}
          >
            <span>{statusLabel(status)}</span>
            <strong>{statusCounts[status].toLocaleString()}</strong>
          </button>
        {/each}
      </div>
      <div class="field-row">
        <input
          aria-label="Search comparisons"
          type="search"
          value={searchText}
          placeholder="Search benchmark, run, machine"
          oninput={(e) => setSearchText(e.currentTarget.value)}
        />
        {#if hardwareOptions.length > 1}
          <select
            aria-label="Machine"
            value={hardwareFilter}
            onchange={(e) => setHardwareFilter(e.currentTarget.value)}
          >
            <option value="all">All machines</option>
            {#each hardwareOptions as hardware}
              <option value={hardware}>{hardware}</option>
            {/each}
          </select>
        {/if}
      </div>
    </section>

    {#if r.missing_run_ids && r.missing_run_ids.length > 0}
      <section class="panel notice">
        <h2>Missing runs</h2>
        <p>{r.missing_run_ids.join(", ")}</p>
      </section>
    {/if}

    {#if filteredRuns.length === 0}
      <section class="panel empty-panel">
        <h2>No comparisons match the current filters</h2>
      </section>
    {:else}
      {#each filteredRuns as entry (entry.run.run_id)}
        {@const run = entry.run}
        {@const comparisons = entry.comparisons}
        {@const missingComparisons = missingBaselineComparisons(comparisons)}
        {@const detailed = detailedComparisons(comparisons)}
        {@const tableComparisons = shownComparisons(run.run_id, detailed)}
        {@const hiddenStable = detailed.length - tableComparisons.length}
        {@const visibleComparisons = tableComparisons.slice(0, rowLimit(run.run_id))}
        {@const runCounts = countStatuses(comparisons)}
        {@const totalComparisons = runComparisons(run).length}
        <section class="panel run" aria-label={`Run ${run.run_id}`}>
          <header class="run-head">
            <div>
              <h2>{run.run_id}</h2>
              <div class="ident">
                {#if run.run_reason}<span>{run.run_reason}</span>{/if}
                {#if run.commit}<span class="mono">{run.commit.sha.slice(0, 8)}</span>{/if}
                {#if run.baseline_run_id}
                  <span>baseline <span class="mono">{run.baseline_run_id}</span>{#if baselineCommitText(run)}{" "}({baselineCommitText(run)}){/if}</span>
                {/if}
              </div>
            </div>
            <div class="run-summary" aria-label={`Summary for ${run.run_id}`}>
              {#if filtersActive()}<span>{plural(comparisons.length, "matching comparison")}</span>{/if}
              {#if runCounts.errored > 0}
                <span>{runCounts.errored.toLocaleString()} benchmark {runCounts.errored === 1 ? "error" : "errors"}</span>
              {/if}
              {#if runCounts.missing_baseline > 0}
                <span>{runCounts.missing_baseline.toLocaleString()} missing baseline</span>
              {/if}
            </div>
          </header>

          {#if run.baseline_error}
            <p class="notice-line">{run.baseline_error.message}</p>
          {/if}

          {#if missingComparisons.length > 0 && statusFilter !== "missing_baseline"}
            <section class="baseline-gap" aria-label={`Missing baseline coverage for ${run.run_id}`}>
              <div>
                <strong>{plural(missingComparisons.length, "benchmark has", "benchmarks have")} no matching baseline result</strong>
                {#if !run.baseline_error}<p>{missingBaselineReason(missingComparisons)}</p>{/if}
              </div>
              <button type="button" onclick={() => setStatusFilter("missing_baseline")}>Show affected benchmarks</button>
            </section>
          {/if}

          {#if tableComparisons.length > 0}
            {#if rowCountText(visibleComparisons.length, tableComparisons.length, totalComparisons)}
              <p class="row-count">
                {rowCountText(visibleComparisons.length, tableComparisons.length, totalComparisons)}
              </p>
            {/if}
            <div class="comparison-list">
              <table class="data-table comparisons">
                <thead>
                  <tr>
                    <th>Status</th>
                    <th>Benchmark</th>
                    <th>Machine</th>
                    <th>Change</th>
                    <th>Z</th>
                    <th>Contender</th>
                    <th>Baseline</th>
                    <th>Links</th>
                  </tr>
                </thead>
                <tbody>
                  {#each visibleComparisons as row}
                    {@const tolerance = row.analysis?.lookback_z_score?.tolerance ?? null}
                    <tr class={`row-${row.status}`}>
                      <td data-label="Status">
                        <div>
                          <span class={`row-status ${row.status}`}>{row.status === "stable" && tolerance?.within_tolerance ? "within tolerance" : statusLabel(row.status)}</span>
                          {#if tolerance?.within_tolerance}
                            <div class="row-note">{toleranceText(tolerance, row.unit)}</div>
                          {/if}
                        </div>
                      </td>
                      <td data-label="Benchmark">
                        <div>
                          <div class="bench-name" title={row.history_fingerprint}>{row.name}</div>
                          {#if row.status === "errored" && row.contender.error}
                            <code class="row-note">{JSON.stringify(row.contender.error)}</code>
                          {:else if row.reason}
                            <div class="row-note">{row.reason}</div>
                          {/if}
                        </div>
                      </td>
                      <td data-label="Machine">{row.hardware.name}</td>
                      <td data-label="Change" class="num">
                        <div>
                          {percentText(row)}
                          {#if row.analysis?.pairwise?.tolerance}<div class="row-note">{signedMeasurement(row.analysis.pairwise.tolerance.delta, row.unit)}</div>{/if}
                        </div>
                      </td>
                      <td data-label="Z" class="num">{zText(row)}</td>
                      <td data-label="Contender" class="num">{valueText(row.contender.single_value_summary, row.unit)}</td>
                      <td data-label="Baseline" class="num">{valueText(row.baseline?.single_value_summary ?? null, row.baseline?.unit ?? null)}</td>
                      <td data-label="Links" class="links">
                        <div>
                          <a href={appURL(row.links.result)} onclick={(e) => go(e, row.links.result)}>result</a>
                          {#if row.links.compare}
                            <a href={appURL(row.links.compare)} onclick={(e) => go(e, row.links.compare!)}>compare</a>
                          {/if}
                          <a href={appURL(row.links.series)} onclick={(e) => go(e, row.links.series)}>series</a>
                        </div>
                      </td>
                    </tr>
                  {/each}
                </tbody>
              </table>
            </div>
            {#if visibleComparisons.length < tableComparisons.length}
              <button type="button" class="button-pill more" onclick={() => showMore(run.run_id)}>Show more</button>
            {/if}
          {/if}
          {#if hiddenStable > 0}
            <button type="button" class="button-pill more" onclick={() => showStable(run.run_id)}>
              Show {plural(hiddenStable, "stable comparison")}
            </button>
          {:else if tableComparisons.length === 0 && missingComparisons.length === 0}
            <p class="empty">No comparison rows.</p>
          {/if}
        </section>
      {/each}
    {/if}
  </div>
{/if}

<style>
  .ci-report-view {
    display: grid;
    gap: 12px;
    min-width: 0;
  }
  .ci-report-view > .summary-line .report-status {
    margin-right: 10px;
  }
  .report-status, .row-status {
    display: inline-flex;
    align-items: center;
    min-height: 20px;
    border-radius: 999px;
    padding: 0 8px;
    font-size: 0.72rem;
    color: var(--c-on-badge);
    background: var(--c-unknown);
  }
  .report-status.success, .row-status.improved {
    background: var(--c-success);
  }
  .report-status.failure, .row-status.regressed, .row-status.errored {
    background: var(--c-error);
  }
  .report-status.action_required, .row-status.missing_baseline, .row-status.not_comparable {
    background: var(--c-warning);
  }
  .report-status.skipped, .row-status.insufficient {
    background: var(--c-insufficient);
    color: var(--c-text-muted);
  }
  .row-status.stable {
    background: var(--c-stable);
  }
  .controls-panel {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    padding: 10px;
  }
  .coverage-panel {
    padding: 0;
    overflow: hidden;
  }
  .coverage-head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 12px;
    padding: 9px 12px;
  }
  .coverage-head h2 {
    margin: 0;
    font-size: 0.95rem;
  }
  .coverage-warning {
    color: var(--c-warn-text);
    font-size: 0.8rem;
    font-weight: 700;
  }
  .coverage-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
    gap: 8px;
    padding: 10px;
    border-top: 1px solid var(--c-border-muted);
  }
  .coverage-card {
    display: grid;
    gap: 6px;
    padding: 8px 10px;
    border: 1px solid var(--c-border-muted);
    border-radius: var(--radius-sm);
    background: var(--c-bg-inset);
  }
  .coverage-card-head {
    display: flex;
    justify-content: space-between;
    gap: 12px;
  }
  .coverage-card-head span {
    color: var(--c-text-muted);
    font-variant-numeric: tabular-nums;
  }
  .coverage-card progress {
    width: 100%;
    height: 6px;
    border: 0;
    accent-color: var(--c-accent);
  }
  .coverage-card p {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    margin: 0;
    color: var(--c-text-muted);
    font-size: 0.75rem;
  }
  .baseline-gap {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 16px;
    margin: 10px 12px 0;
    padding: 10px;
    border: 1px solid color-mix(in srgb, var(--c-warning) 50%, var(--c-border-muted));
    border-radius: var(--radius-sm);
    background: var(--c-warn-bg);
    color: var(--c-warn-text);
  }
  .baseline-gap p {
    margin: 3px 0 0;
    font-size: 0.78rem;
  }
  .baseline-gap button {
    flex: none;
    min-height: 30px;
    border: 1px solid currentColor;
    border-radius: var(--radius-sm);
    padding: 0 9px;
    background: transparent;
    color: inherit;
    cursor: pointer;
    font: inherit;
    font-size: 0.78rem;
    font-weight: 700;
  }
  .status-tabs {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .status-tabs button {
    min-height: 30px;
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 0 9px;
    border: 1px solid var(--c-border-muted);
    border-radius: var(--radius-sm);
    background: var(--c-surface);
    color: var(--c-text-muted);
    cursor: pointer;
    font-weight: 650;
  }
  .status-tabs button[aria-pressed="true"] {
    border-color: var(--c-accent);
    color: var(--c-accent);
    background: color-mix(in srgb, var(--c-accent) 8%, var(--c-surface));
  }
  .status-tabs strong {
    color: var(--c-text);
    font-variant-numeric: tabular-nums;
  }
  .field-row {
    display: flex;
    flex: 1 1 320px;
    justify-content: flex-end;
    gap: 8px;
  }
  .field-row input {
    flex: 0 1 300px;
    min-width: 0;
  }
  .field-row input, .field-row select {
    min-height: 32px;
    padding: 0 8px;
    border: 1px solid var(--c-border-muted);
    border-radius: var(--radius-sm);
    background: var(--c-surface);
    color: var(--c-text);
    font-size: 0.82rem;
  }
  .notice {
    padding: 12px;
    background: var(--c-warn-bg);
    color: var(--c-warn-text);
  }
  .notice h2 {
    margin-bottom: 4px;
    font-size: 0.95rem;
  }
  .notice p {
    margin: 0;
  }
  .run {
    overflow: hidden;
  }
  .run-head {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 12px;
    padding: 12px;
    border-bottom: 1px solid var(--c-border-muted);
  }
  .run h2 {
    margin: 0 0 4px;
    font-size: 1rem;
  }
  .ident {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
    color: var(--c-text-muted);
    font-size: 0.78rem;
  }
  .run-summary {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: 6px;
    color: var(--c-text-muted);
    font-size: 0.74rem;
  }
  .run-summary span {
    min-height: 24px;
    display: inline-flex;
    align-items: center;
    padding: 0 7px;
    border: 1px solid var(--c-border-muted);
    border-radius: 999px;
    background: var(--c-bg-inset);
  }
  .notice-line {
    margin: 10px 12px 0;
    color: var(--c-warn-text);
    background: var(--c-warn-bg);
    border-radius: var(--radius-sm);
    padding: 8px;
  }
  .row-count {
    margin: 10px 12px 6px;
    color: var(--c-text-muted);
    font-size: 0.78rem;
  }
  .comparison-list {
    max-width: 100%;
  }
  .comparisons td {
    overflow-wrap: anywhere;
  }
  .row-regressed td, .row-errored td {
    background: color-mix(in srgb, var(--c-error) 5%, transparent);
  }
  .row-missing_baseline td, .row-not_comparable td {
    background: color-mix(in srgb, var(--c-warning) 5%, transparent);
  }
  .bench-name {
    font-weight: 700;
  }
  .row-note {
    display: block;
    margin-top: 3px;
    color: var(--c-text-muted);
    font-size: 0.74rem;
    font-weight: 400;
  }
  .num {
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .links {
    white-space: nowrap;
  }
  .links a {
    margin-right: 7px;
    color: var(--c-accent);
  }
  .more {
    margin: 10px 12px 12px;
  }
  .empty-panel {
    padding: 18px;
  }
  .empty-panel h2 {
    margin: 0;
    font-size: 0.96rem;
  }
  .empty, .error {
    color: var(--c-text-muted);
  }
  .error {
    color: var(--c-error);
  }
  @media (max-width: 1080px) {
    .run-head {
      flex-direction: column;
    }
    .run-summary {
      justify-content: flex-start;
    }
  }
  @media (max-width: 820px) {
    .coverage-head, .baseline-gap {
      align-items: flex-start;
      flex-direction: column;
    }
    .field-row {
      flex-direction: column;
    }
    .field-row input {
      flex: none;
    }
    .comparisons {
      min-width: 0;
    }
    .comparisons, .comparisons thead, .comparisons tbody, .comparisons tr, .comparisons th, .comparisons td {
      display: block;
    }
    .comparisons thead {
      display: none;
    }
    .comparisons tr {
      padding: 8px 10px;
      border-bottom: 1px solid var(--c-border-muted);
    }
    .comparisons td {
      display: grid;
      grid-template-columns: 88px minmax(0, 1fr);
      gap: 8px;
      padding: 4px 0;
      border-bottom: 0;
    }
    .comparisons td::before {
      content: attr(data-label);
      color: var(--c-text-muted);
      font-size: 0.68rem;
      text-transform: uppercase;
      letter-spacing: 0.04em;
      font-weight: 750;
    }
  }
</style>
