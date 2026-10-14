<script lang="ts">
  import { appURL } from "../base-path";
  import type { BrowseRow, SortKey, SortSpec } from "../browse/transform";
  import { interceptNavClick } from "../router";
  import MeasurementValue from "./MeasurementValue.svelte";
  import BenchmarkStatus from "./BenchmarkStatus.svelte";

  let {
    rows,
    sort = null,
    onsort,
    onopen,
  }: {
    rows: BrowseRow[];
    sort?: SortSpec | null;
    onsort?: (key: SortKey) => void;
    onopen?: (row: BrowseRow) => void;
  } = $props();

  const HEADERS: { key: SortKey | null; label: string }[] = [
    { key: "name", label: "benchmark" },
    { key: "svs", label: "last value" },
    { key: "points", label: "history" },
    { key: null, label: "status" },
    { key: "commit", label: "last commit" },
  ];

  function arrow(key: SortKey): string {
    if (sort?.key !== key) return "";
    return sort.dir === "asc" ? " ^" : " v";
  }

  function machinesText(row: BrowseRow): string {
    return `${row.machineNames.length} ${row.machineNames.length === 1 ? "machine" : "machines"}`;
  }

  function historyText(row: BrowseRow): string {
    return `${row.pointCount} points · ${machinesText(row)}`;
  }

  function ariaSort(key: SortKey | null): "ascending" | "descending" | undefined {
    if (key === null) return undefined;
    if (sort?.key !== key) return undefined;
    return sort.dir === "asc" ? "ascending" : "descending";
  }
</script>

<div class="table-panel browse-table-panel">
  <table class="data-table stacked-table browse-table">
    <colgroup>
      <col class="benchmark-col" />
      <col class="value-col" />
      <col class="points-col" />
      <col class="status-col" />
      <col class="commit-col" />
    </colgroup>
    <thead>
      <tr>
        {#each HEADERS as h (h.label)}
          <th aria-sort={ariaSort(h.key)}>
            {#if h.key !== null}
              {@const key = h.key}
              <button
                type="button"
                class="sort"
                aria-label={`Sort by ${h.label}`}
                aria-pressed={sort?.key === key}
                class:active={sort?.key === key}
                onclick={() => onsort?.(key)}
              >
                <span>{h.label}</span><span aria-hidden="true">{arrow(key)}</span>
              </button>
            {:else}
              {h.label}
            {/if}
          </th>
        {/each}
      </tr>
    </thead>
    <tbody>
      {#each rows as row (row.benchmarkId)}
        <!-- The whole row is a pointer convenience. It keeps its table-row role so
             screen readers still read its cells; the name <a> below is the
             keyboard and screen-reader way to open the benchmark. -->
        <tr onclick={(e) => interceptNavClick(e) && onopen?.(row)}>
          <td class="name-cell" data-label="benchmark">
            <span class="identity-stack">
              <a
                class="row-primary-link"
                href={appURL(`/benchmarks/${row.benchmarkId}`)}
                onclick={(e) => {
                  if (!onopen || !interceptNavClick(e)) return;
                  e.preventDefault();
                  e.stopPropagation();
                  onopen(row);
                }}>{row.name}</a>
              {#if row.paramsText}<span class="metadata-line">{row.paramsText}</span>{/if}
              <!-- Narrow screens hide the value, history, and commit columns and
                   show this one line instead, so each row stays two lines. -->
              <span class="narrow-meta">
                {row.svsText} · {historyText(row)} · <span class="mono">{row.commitSha}</span> · {row.commitDateText}
              </span>
            </span>
          </td>
          <td class="num-cell" data-label="last value"><MeasurementValue value={row.latestSVS} unit={row.unit} /></td>
          <td class="history-cell" data-label="history">
            <span class="history-stack">
              <span>{row.pointCount} points</span>
              <span class="metadata-line">{machinesText(row)}</span>
            </span>
          </td>
          <td class="status-cell" data-label="status"><BenchmarkStatus {row} /></td>
          <td class="muted commit-cell" data-label="last commit"><span class="mono">{row.commitSha}</span> · {row.commitDateText}</td>
        </tr>
      {/each}
    </tbody>
  </table>
</div>

<style>
  .browse-table-panel {
    --stacked-label-width: 92px;
  }

  .browse-table {
    min-width: 820px;
  }

  .benchmark-col {
    width: 26%;
  }

  .value-col {
    width: 16%;
  }

  .points-col {
    width: 14%;
  }

  .status-col {
    width: 12%;
  }

  .commit-col {
    width: 13%;
  }

  .browse-table tbody tr {
    cursor: pointer;
  }

  .name-cell {
    min-width: 0;
  }

  .identity-stack {
    display: grid;
    gap: 3px;
    min-width: 0;
    overflow-wrap: anywhere;
  }

  .metadata-line {
    display: block;
    color: var(--c-text-faint);
    font-size: 0.74rem;
    line-height: 1.3;
    overflow-wrap: anywhere;
  }

  .muted {
    color: var(--c-text-muted);
  }

  .num-cell,
  .status-cell {
    white-space: nowrap;
  }

  .commit-cell {
    white-space: normal;
  }

  .num-cell {
    font-variant-numeric: tabular-nums;
  }

  .history-cell {
    font-variant-numeric: tabular-nums;
  }
  .history-cell span {
    display: block;
  }

  .sort {
    background: none;
    border: none;
    padding: 0;
    font: inherit;
    color: inherit;
    text-transform: inherit;
    letter-spacing: inherit;
    cursor: pointer;
    text-align: left;
  }

  .sort.active {
    color: var(--c-accent);
  }

  .history-stack {
    display: grid;
    gap: 2px;
  }

  .narrow-meta {
    display: none;
  }

  /* Below the stacked-table breakpoint each row is two parts: the name with a
     one-line summary under it, and the status beside it. */
  @media (max-width: 1120px) {
    .browse-table {
      min-width: 0;
    }

    .browse-table tbody tr {
      display: grid;
      grid-template-columns: minmax(0, 1fr) auto;
      align-items: start;
      gap: 0 12px;
    }

    .browse-table td {
      display: block;
      padding: 0;
    }

    .browse-table td::before {
      content: none;
    }

    .browse-table .num-cell,
    .browse-table .history-cell,
    .browse-table .commit-cell {
      display: none;
    }

    .narrow-meta {
      display: block;
      color: var(--c-text-muted);
      font-size: 0.78rem;
      font-variant-numeric: tabular-nums;
      overflow-wrap: anywhere;
    }

    .status-cell {
      white-space: normal;
    }
  }
</style>
