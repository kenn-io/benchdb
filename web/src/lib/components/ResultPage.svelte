<script lang="ts">
  import { onMount } from "svelte";

  import { createBenchDBClient } from "../api/client";
  import { formatMeasurement } from "../format";
  import {
    loadResult,
    loadResultHistory,
    resultViewModelFromDetail,
    type ResultViewModel,
  } from "../result/loader";
  import { interceptNavClick, navigate } from "../router";
  import { flagsText, type SeriesPoint } from "../series/transform";
  import EnvironmentDetails from "./EnvironmentDetails.svelte";
  import MeasurementValue from "./MeasurementValue.svelte";
  import SeriesChart from "./SeriesChart.svelte";

  let {
    resultId,
    baseUrl = "",
  }: {
    resultId: string;
    baseUrl?: string;
  } = $props();

  const client = $derived(createBenchDBClient(baseUrl));

  let vm = $state<ResultViewModel | null>(null);
  let historyPoints = $state<SeriesPoint[]>([]);
  let historyError = $state<string | null>(null);
  let errorMsg = $state<string | null>(null);
  let actionMsg = $state<string | null>(null);
  let actionError = $state<string | null>(null);
  let busyAction = $state<"annotation" | "delete" | null>(null);
  let canWrite = $state(false);
  let deleted = $state(false);

  onMount(async () => {
    void loadWriteCapability();
    const [resultOutcome, historyOutcome] = await Promise.allSettled([
      loadResult(client, resultId),
      loadResultHistory(client, resultId),
    ]);
    if (resultOutcome.status === "rejected") {
      errorMsg = resultOutcome.reason instanceof Error
        ? resultOutcome.reason.message
        : String(resultOutcome.reason);
      return;
    }
    vm = resultOutcome.value;
    if (historyOutcome.status === "fulfilled") {
      historyPoints = historyOutcome.value;
    } else {
      historyError = historyOutcome.reason instanceof Error
        ? historyOutcome.reason.message
        : String(historyOutcome.reason);
    }
  });

  let currentIndex = $derived(historyPoints.findIndex((point) => point.resultId === resultId));
  let currentPoint = $derived(currentIndex < 0 ? null : (historyPoints[currentIndex] ?? null));
  let previousPoint = $derived(currentIndex <= 0 ? null : (historyPoints[currentIndex - 1] ?? null));
  let comparison = $derived(comparisonFrom(currentPoint, previousPoint, vm));

  async function loadWriteCapability() {
    try {
      const res = await client.GET("/api/auth/capabilities");
      canWrite = !res.error && res.data?.can_write_results === true;
    } catch {
      canWrite = false;
    }
  }

  async function setDistributionChange(value: boolean) {
    if (vm === null || busyAction !== null) return;
    actionMsg = null;
    actionError = null;
    busyAction = "annotation";
    try {
      const res = await client.PUT("/api/benchmark-results/{id}", {
        params: { path: { id: vm.id } },
        body: {
          change_annotations: {
            begins_distribution_change: value ? true : null,
          },
        },
      });
      if (res.error || !res.data) {
        actionError = detailOf(res.error, "failed to update annotations");
        return;
      }
      vm = resultViewModelFromDetail(res.data);
      actionMsg = "annotation updated";
    } catch (err) {
      actionError = err instanceof Error ? err.message : String(err);
    } finally {
      busyAction = null;
    }
  }

  async function deleteResult() {
    if (vm === null || busyAction !== null) return;
    if (!window.confirm(`Delete result ${vm.id}?`)) return;
    actionMsg = null;
    actionError = null;
    busyAction = "delete";
    try {
      const res = await client.DELETE("/api/benchmark-results/{id}", {
        params: { path: { id: vm.id } },
      });
      if (res.error) {
        actionError = detailOf(res.error, "failed to delete result");
        return;
      }
      deleted = true;
      vm = null;
      actionMsg = "result deleted";
    } catch (err) {
      actionError = err instanceof Error ? err.message : String(err);
    } finally {
      busyAction = null;
    }
  }

  function detailOf(error: unknown, fallback: string): string {
    if (error && typeof error === "object" && "detail" in error && typeof error.detail === "string") {
      return error.detail;
    }
    return fallback;
  }

  function go(e: MouseEvent, href: string) {
    if (!interceptNavClick(e)) return;
    e.preventDefault();
    navigate(href);
  }

  function openHistoryResult(historyResultId: string) {
    if (historyResultId !== resultId) {
      navigate(`/results/${historyResultId}`);
    }
  }

  function comparisonFrom(
    current: SeriesPoint | null,
    previous: SeriesPoint | null,
    result: ResultViewModel | null,
  ): { headline: string; detail: string; tone: "good" | "bad" | "neutral" } {
    if (current === null) {
      return {
        headline: "Not in default-branch history",
        detail: "This result is not part of the comparable series history.",
        tone: "neutral",
      };
    }
    if (previous === null) {
      return {
        headline: "First recorded point",
        detail: "There is no earlier result in this series to compare.",
        tone: "neutral",
      };
    }
    const delta = current.svs - previous.svs;
    const percent = previous.svs === 0 ? null : (delta / Math.abs(previous.svs)) * 100;
    const direction = delta === 0 ? "unchanged" : delta > 0 ? "higher" : "lower";
    let verdict = direction;
    let tone: "good" | "bad" | "neutral" = "neutral";
    if (delta !== 0 && result?.lessIsBetter !== null && result?.lessIsBetter !== undefined) {
      const better = result.lessIsBetter ? delta < 0 : delta > 0;
      verdict = better ? "better" : "worse";
      tone = better ? "good" : "bad";
    }
    const percentText = percent === null ? "" : `${Math.abs(percent).toFixed(1)}% `;
    return {
      headline: delta === 0 ? "Unchanged from previous" : `${percentText}${verdict} than previous`,
      detail: `${formatMeasurement(current.svs, current.unit)} vs ${formatMeasurement(previous.svs, previous.unit)} at ${previous.commitHash.slice(0, 8)}`,
      tone,
    };
  }
</script>

{#if errorMsg}
  <main class="page result-page">
    <section class="panel state-panel">
      <h1>Result unavailable</h1>
      <p class="error">Failed to load result: {errorMsg}</p>
    </section>
  </main>
{:else if deleted}
  <main class="page result-page">
    <section class="panel state-panel">
      <h1>Result deleted</h1>
      {#if actionMsg}<p>{actionMsg}</p>{/if}
      <div class="action-row">
        <a class="button-pill primary" href="/series" onclick={(e) => go(e, "/series")}>Browse series</a>
      </div>
    </section>
  </main>
{:else if !vm}
  <main class="page result-page">
    <section class="panel state-panel">
      <h1>Loading result</h1>
      <p>Loading...</p>
    </section>
  </main>
{:else}
  {@const seriesHref = `/series/${vm.benchmarkId}`}
  <main class="page result-page">
    <header class="page-header">
      <div>
        <p class="eyebrow">Benchmark result</p>
        <h1>{vm.name}</h1>
        <p class="page-subtitle result-subtitle">
          {#if vm.paramsText}<span>{vm.paramsText}</span>{:else}<span>No benchmark parameters</span>{/if}
        </p>
      </div>
      <div class="header-actions">
        <div class="page-meta">
          <span>result value <span class="numeric-text"><MeasurementValue value={vm.svs} unit={vm.unit} /></span></span>
          <span>machine {vm.hardwareName}</span>
          {#if vm.commitSha !== null}<span title={vm.commitSha}>commit {vm.shortCommit}</span>{/if}
        </div>
        <div class="action-row">
          <a
            class="button-pill"
            href={seriesHref}
            onclick={(e) => go(e, seriesHref)}
          >Explore full series</a>
          <a class="button-pill" href={vm.historyExportHref} download={`benchdb-history-${vm.id}.json`}>
            Export history JSON
          </a>
        </div>
      </div>
    </header>

    <section class="panel trend-hero" aria-label="Result in series trend">
      <div class="trend-heading">
        <div>
          <p class="eyebrow">Series trend</p>
          <h2>This result in context</h2>
        </div>
        <div class={`comparison ${comparison.tone}`}>
          <strong>{comparison.headline}</strong>
          <span>{comparison.detail}</span>
        </div>
      </div>
      {#if historyError !== null}
        <p class="error">Trend unavailable: {historyError}</p>
      {:else if historyPoints.length === 0}
        <p class="empty-history">Loading series history…</p>
      {:else}
        <SeriesChart
          points={historyPoints}
          sigma={2}
          height={320}
          currentResultId={resultId}
          onopen={openHistoryResult}
        />
        <div class="trend-foot">
          <span>{historyPoints.length} {historyPoints.length === 1 ? "result" : "results"} in this series</span>
          {#if currentPoint !== null}
            <span>{currentPoint.commitHash.slice(0, 8)} · <MeasurementValue value={currentPoint.svs} unit={currentPoint.unit} /></span>
            {#if flagsText(currentPoint.stats) !== ""}<span class="flag">{flagsText(currentPoint.stats)}</span>{/if}
          {/if}
        </div>
      {/if}
    </section>

    {#if canWrite}
      <section class="panel action-panel" aria-label="Result actions">
        <h2>Actions</h2>
        <div class="action-row">
          {#if vm.beginsDistributionChange}
            <button
              type="button"
              class="button-pill"
              onclick={() => setDistributionChange(false)}
              disabled={busyAction !== null}
            >
              {busyAction === "annotation" ? "Saving..." : "Unmark distribution change"}
            </button>
          {:else}
            <button
              type="button"
              class="button-pill"
              onclick={() => setDistributionChange(true)}
              disabled={busyAction !== null}
            >
              {busyAction === "annotation" ? "Saving..." : "Mark distribution change"}
            </button>
          {/if}
          <button type="button" class="button-pill danger" onclick={deleteResult} disabled={busyAction !== null}>
            {busyAction === "delete" ? "Deleting..." : "Delete result"}
          </button>
        </div>
        {#if actionMsg}<p class="ok">{actionMsg}</p>{/if}
        {#if actionError}<p class="error">{actionError}</p>{/if}
      </section>
    {/if}

    <section class="panel result-section measurement-section" aria-label="Result measurement">
      <div class="measurement-primary">
        <span class="eyebrow">{vm.svsType}</span>
        <strong class="numeric-text">{vm.svsText}</strong>
        <span>{vm.lessIsBetter === null ? "Direction not set" : vm.lessIsBetter ? "Lower is better" : "Higher is better"}</span>
      </div>
      <dl class="compact-dl measurement-details">
        {#if vm.iterations !== null}
          <dt>iterations</dt>
          <dd class="numeric-text">{vm.iterations}</dd>
        {/if}
        {#each vm.aggregates as agg (agg.label)}
          <dt>{agg.label}</dt>
          <dd class="numeric-text">{agg.value}</dd>
        {/each}
        <dt>time unit</dt>
        <dd>{vm.timeUnitText}</dd>
        <dt>raw data</dt>
        <dd>{vm.dataCountText}</dd>
        <dt>raw times</dt>
        <dd>{vm.timesCountText}</dd>
      </dl>
      {#if vm.error !== null}
        <div class="errbox" role="alert">
          <h3>error</h3>
          <pre>{JSON.stringify(vm.error, null, 2)}</pre>
        </div>
      {/if}
    </section>

    <section class="result-facts" aria-label="Result facts">
      <div class="panel result-section">
        <h2>Commit</h2>
        <dl class="compact-dl">
          {#if vm.commitSha !== null}
            <dt>sha</dt>
            <dd class="mono" title={vm.commitSha}>{vm.shortCommit}</dd>
          {/if}
          {#if vm.commitMessage !== null && vm.commitMessage !== ""}
            <dt>message</dt>
            <dd>{vm.commitMessage}</dd>
          {/if}
          {#if vm.commitDateText !== null}
            <dt>date</dt>
            <dd>{vm.commitDateText}</dd>
          {/if}
          <dt>repository</dt>
          <dd title={vm.repository}>{vm.repositoryLabel}</dd>
        </dl>
      </div>

      <div class="panel result-section">
        <h2>Run</h2>
        <dl class="compact-dl">
          <dt>run id</dt>
          <dd class="mono" title={vm.runId}>{vm.displayRunId}</dd>
          {#if vm.runTagsText}
            <dt>run tags</dt>
            <dd>{vm.runTagsText}</dd>
          {/if}
          {#if vm.runReason !== null}
            <dt>reason</dt>
            <dd>{vm.runReason}</dd>
          {/if}
          {#if vm.batchId !== null}
            <dt>batch</dt>
            <dd class="mono" title={vm.batchId}>{vm.displayBatchId}</dd>
          {/if}
          <dt>result time</dt>
          <dd>{vm.resultDateText}</dd>
          <dt>result id</dt>
          <dd class="mono" title={vm.id}>{vm.displayResultId}</dd>
        </dl>
      </div>

      <div class="panel result-section">
        <h2>Machine</h2>
        <dl class="compact-dl">
          <dt>name</dt>
          <dd>{vm.hardwareName}</dd>
          <dt>kind</dt>
          <dd>{vm.hardwareType}</dd>
        </dl>
      </div>
    </section>

    <details class="panel technical-disclosure" aria-label="Technical details">
      <summary>
        <span>
          <strong>Technical details</strong>
          <small>Environment, identifiers, and submitted payloads</small>
        </span>
        <span class="disclosure-hint">Show</span>
      </summary>
      <div class="technical-body">
        <div class="json-grid">
          <EnvironmentDetails context={vm.context} />
          <details class="json-panel">
            <summary>Identifiers</summary>
            <dl class="compact-dl identifier-list">
              <dt>machine identity</dt>
              <dd class="mono" title={vm.hardwareHash}>{vm.displayHardwareHash}</dd>
              <dt>series</dt>
              <dd class="mono" title={vm.fingerprint}>{vm.displayFingerprint}</dd>
              <dt>benchmark</dt>
              <dd class="mono" title={vm.benchmarkId}>{vm.displayBenchmarkId}</dd>
            </dl>
          </details>
          {#each vm.jsonBlocks as block (block.label)}
            <details class="json-panel">
              <summary>{block.label}</summary>
              <pre>{block.value}</pre>
            </details>
          {/each}
        </div>
      </div>
    </details>
</main>
{/if}

<style>
  .result-page {
    max-width: 1400px;
  }
  .header-actions {
    display: grid;
    justify-items: end;
    gap: 8px;
    min-width: min(100%, 420px);
  }
  .result-subtitle {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
  }
  .trend-hero {
    padding: 12px;
  }
  .trend-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 18px;
    margin-bottom: 10px;
  }
  .trend-heading h2 {
    margin: 0;
    font-size: 1rem;
  }
  .comparison {
    display: grid;
    justify-items: end;
    gap: 1px;
    max-width: 520px;
    text-align: right;
    color: var(--c-text-muted);
    font-size: 0.76rem;
  }
  .comparison strong {
    color: var(--c-text);
    font-size: 0.9rem;
  }
  .comparison.good strong { color: var(--c-success); }
  .comparison.bad strong { color: var(--c-error); }
  .trend-foot {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    margin-top: 8px;
    color: var(--c-text-muted);
    font-size: 0.76rem;
  }
  .trend-foot span + span::before {
    content: "·";
    margin-right: 8px;
    color: var(--c-border);
  }
  .trend-foot .flag {
    color: var(--c-warning);
    font-weight: 700;
  }
  .empty-history {
    min-height: 280px;
    display: grid;
    place-items: center;
    margin: 0;
    color: var(--c-text-muted);
  }
  .action-panel,
  .result-section {
    padding: 12px;
  }
  .action-panel h2,
  .result-section h2 {
    margin: 0 0 8px;
    font-size: 0.95rem;
  }
  .measurement-section {
    display: grid;
    grid-template-columns: minmax(180px, 0.45fr) minmax(0, 1fr);
    gap: 24px;
    align-items: start;
  }
  .measurement-primary {
    display: grid;
    align-content: start;
    gap: 3px;
  }
  .measurement-primary strong {
    font-size: clamp(1.65rem, 4vw, 2.5rem);
    line-height: 1.08;
  }
  .measurement-primary > span:last-child {
    color: var(--c-text-muted);
    font-size: 0.76rem;
  }
  .identifier-list {
    padding: 10px;
  }
  .result-facts {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 10px;
  }
  .compact-dl {
    display: grid;
    grid-template-columns: minmax(7rem, max-content) minmax(0, 1fr);
    align-items: baseline;
    gap: 0.25rem 1rem;
    font-size: 0.85rem;
    margin: 0;
  }
  .measurement-details {
    grid-template-columns: max-content minmax(0, 1fr) max-content minmax(0, 1fr);
    column-gap: 1rem;
  }
  dt { color: var(--c-text-muted); }
  dd { margin: 0; font-variant-numeric: tabular-nums; overflow-wrap: anywhere; }
  .ok { color: var(--c-success); }
  .technical-disclosure {
    min-width: 0;
  }
  .technical-disclosure > summary {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 16px;
    margin: 0;
    padding: 11px 12px;
    border: 0;
    list-style: none;
  }
  .technical-disclosure > summary::-webkit-details-marker {
    display: none;
  }
  .technical-disclosure > summary > span:first-child {
    display: grid;
    gap: 1px;
  }
  .technical-disclosure small {
    color: var(--c-text-muted);
    font-weight: 400;
  }
  .technical-disclosure[open] .disclosure-hint::after {
    content: " less";
  }
  .technical-disclosure:not([open]) .disclosure-hint::after {
    content: " details";
  }
  .technical-body {
    padding: 0 12px 12px;
  }
  .json-grid {
    min-width: 0;
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 0.75rem;
  }
  summary {
    margin: -10px -10px 0;
    padding: 0.45rem 0.6rem;
    border-bottom: 1px solid var(--c-border-muted);
    color: var(--c-text-muted);
    font-size: 0.78rem;
    font-weight: 700;
    cursor: pointer;
  }
  pre {
    max-width: 100%;
    max-height: 340px;
    margin: 0;
    padding: 0.55rem 0.6rem;
    overflow: auto;
    background: var(--c-bg-inset);
    font-size: 0.74rem;
    line-height: 1.4;
  }
  .errbox { margin-top: 0.6rem; padding: 0.5rem 0.7rem; border: 1px solid var(--c-error);
            border-radius: 4px; background: var(--c-warn-bg); font-size: 0.8rem; }
  .errbox h3 { margin: 0 0 0.3rem; font-size: 0.8rem; color: var(--c-error); }
  .errbox pre { margin: 0; white-space: pre-wrap; }
  .error { color: var(--c-error); }
  .state-panel h1 {
    margin-bottom: 6px;
  }
  @media (max-width: 760px) {
    .header-actions {
      justify-items: stretch;
    }
    .trend-heading {
      display: grid;
    }
    .comparison {
      justify-items: start;
      text-align: left;
    }
    .measurement-section,
    .result-facts,
    .json-grid { grid-template-columns: 1fr; }
    .compact-dl {
      grid-template-columns: minmax(6.5rem, 9rem) minmax(0, 1fr);
      column-gap: 0.75rem;
    }
    .measurement-details {
      grid-template-columns: minmax(6.5rem, 9rem) minmax(0, 1fr);
    }
  }
</style>
