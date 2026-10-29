<script lang="ts">
  import { PageHeader } from "@kenn-io/kit-ui";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import { onMount } from "svelte";

  import type { StorageStatus } from "../api/benchdb";
  import { createBenchDBClient } from "../api/client";
  import { formatBytes, formatNumber } from "../format";

  let { baseUrl = "" }: { baseUrl?: string } = $props();
  const client = $derived(createBenchDBClient(baseUrl));
  let status = $state<StorageStatus | null>(null);
  let loading = $state(true);
  let error = $state(false);
  let request: AbortController | null = null;

  const stateLabels: Record<string, string> = {
    scheduled: "Scheduled", running: "Running", failed: "Failed", interrupted: "Interrupted", disabled: "Disabled",
  };
  const phaseLabels: Record<string, string> = {
    idle: "Idle", export: "Exporting results", compact: "Compacting files", catalogs: "Refreshing catalogs",
    cleanup: "Removing old files", schedule: "Saving schedule",
  };
  const dateFormat = new Intl.DateTimeFormat(undefined, {
    timeZone: "UTC", dateStyle: "medium", timeStyle: "short",
  });

  onMount(() => {
    void load();
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") void load();
    }, 30_000);
    return () => {
      window.clearInterval(timer);
      request?.abort();
    };
  });

  async function load() {
    if (request !== null) return;
    const controller = new AbortController();
    request = controller;
    loading = true;
    error = false;
    try {
      const response = await client.storageStatus({ signal: controller.signal });
      if (controller.signal.aborted) return;
      if (response.status === 200) status = response.data;
      else error = true;
    } catch {
      if (!controller.signal.aborted) error = true;
    } finally {
      request = null;
      if (!controller.signal.aborted) loading = false;
    }
  }

  function intervalText(seconds: number): string {
    return `${formatNumber(seconds / 3600)} ${seconds === 3600 ? "hour" : "hours"}`;
  }
</script>

{#snippet timestamp(value: string | null | undefined, missing = "—")}
  {#if value}
    <time datetime={value}>{dateFormat.format(new Date(value))} UTC</time>
  {:else}
    {missing}
  {/if}
{/snippet}

<main class="page storage-page">
  <PageHeader title="Storage">
    {#snippet actions()}
      <button type="button" class="button-pill refresh" onclick={load} disabled={loading}>
        <RefreshCwIcon size={13} strokeWidth={1.75} aria-hidden="true" />
        Refresh
      </button>
    {/snippet}
  </PageHeader>

  {#if loading && status === null}
    <p class="panel state-panel loading" role="status">Loading storage…</p>
  {/if}
  {#if error}
    <p class="panel state-panel error" role="alert">
      {status === null ? "Could not load storage." : "Could not refresh storage. Showing the last available sample."}
    </p>
  {/if}

  {#if status !== null}
    {@const maintenance = status.maintenance}
    <div class="panel storage-grid">
      <section aria-labelledby="postgres-heading">
        <h2 id="postgres-heading">PostgreSQL</h2>
        <dl>
          <div><dt>Database size</dt><dd>{formatBytes(status.postgres_bytes)}</dd></div>
          <div><dt>Pending results (estimate)</dt><dd>{formatNumber(status.pending_rows_estimate)}</dd></div>
        </dl>
      </section>
      <section aria-labelledby="parquet-heading">
        <h2 id="parquet-heading">Parquet archive</h2>
        <dl>
          <div><dt>Size</dt><dd>{formatBytes(status.parquet_bytes)}</dd></div>
          <div><dt>Archived rows</dt><dd>{formatNumber(status.archived_rows)}</dd></div>
          <div><dt>Files</dt><dd>{formatNumber(status.parquet_files)}</dd></div>
        </dl>
      </section>
      <section aria-labelledby="catalogs-heading">
        <h2 id="catalogs-heading">Catalogs</h2>
        <dl>
          <div><dt>Size</dt><dd>{formatBytes(status.catalog_bytes)}</dd></div>
          <div><dt>Files</dt><dd>{formatNumber(status.catalog_files)}</dd></div>
        </dl>
      </section>
    </div>
    <p class="sample-time">
      Storage sampled {@render timestamp(status.sampled_at)}. Totals may be up to a minute old.
    </p>

    <section class="panel section-panel" aria-labelledby="volume-heading">
      <h2 id="volume-heading">Archive volume</h2>
      <dl class="volume-values">
        <div><dt>Available</dt><dd>{formatBytes(status.volume_available_bytes)}</dd></div>
        <div><dt>Total capacity</dt><dd>{formatBytes(status.volume_total_bytes)}</dd></div>
      </dl>
      {#if status.volume_total_bytes > 0}
        <meter
          aria-label="Archive volume used"
          min="0"
          max={status.volume_total_bytes}
          value={status.volume_total_bytes - status.volume_available_bytes}
        ></meter>
      {/if}
      <p class="note">Includes other files on the same volume.</p>
    </section>

    <section class="panel section-panel" aria-labelledby="maintenance-heading">
      <header class="section-header">
        <h2 id="maintenance-heading">Maintenance</h2>
        <span
          class="status-badge"
          class:success={maintenance.state === "running"}
          class:error={maintenance.state === "failed"}
          class:warning={maintenance.state === "interrupted"}
          aria-live="polite"
        >{stateLabels[maintenance.state] ?? maintenance.state}</span>
      </header>
      {#if maintenance.error}
        <p class="error" role="alert">{maintenance.error}</p>
      {/if}
      <dl class="maintenance-details">
        {#if maintenance.enabled}
          <div>
            <dt>Schedule</dt>
            <dd>Every {intervalText(maintenance.interval_seconds)}</dd>
          </div>
        {/if}
        <div>
          <dt>Next run</dt>
          <dd>
            {#if maintenance.state === "running"}
              {intervalText(maintenance.interval_seconds)} after this cycle
            {:else}
              {@render timestamp(maintenance.next_run_at, "Not scheduled")}
            {/if}
          </dd>
        </div>
        <div><dt>Last success</dt><dd>{@render timestamp(maintenance.last_success_at, "Not yet")}</dd></div>
        {#if maintenance.phase !== "idle"}
          <div><dt>Phase</dt><dd>{phaseLabels[maintenance.phase] ?? maintenance.phase}</dd></div>
        {/if}
        <div><dt>Pending edits</dt><dd>{formatNumber(status.pending_mutations)}</dd></div>
        <div><dt>Files awaiting catalog refresh</dt><dd>{formatNumber(status.stale_catalogs)}</dd></div>
      </dl>
      <p class="note">New results remain available before export.</p>

      {#if maintenance.last_started_at}
        <section class="cycle" aria-labelledby="cycle-heading">
          <h3 id="cycle-heading">{maintenance.state === "running" ? "Current cycle" : "Latest cycle"}</h3>
          <dl class="maintenance-details">
            <div><dt>Started</dt><dd>{@render timestamp(maintenance.last_started_at)}</dd></div>
            <div><dt>Finished</dt><dd>{@render timestamp(maintenance.last_finished_at)}</dd></div>
          </dl>
          <dl class="cycle-counts">
            <div><dt>Results exported</dt><dd>{formatNumber(maintenance.exported_rows)}</dd></div>
            <div><dt>Files compacted</dt><dd>{formatNumber(maintenance.compacted_files)}</dd></div>
            <div><dt>Files with refreshed catalogs</dt><dd>{formatNumber(maintenance.refreshed_catalogs)}</dd></div>
            <div><dt>Old files removed</dt><dd>{formatNumber(maintenance.removed_files)}</dd></div>
          </dl>
        </section>
      {/if}
    </section>
  {/if}
</main>

<style>
  .storage-page {
    max-width: 1100px;
  }

  .section-header {
    align-items: center;
  }

  .refresh,
  .section-header {
    display: flex;
    gap: 8px;
  }

  .section-header {
    justify-content: space-between;
    margin-bottom: 12px;
  }

  h2,
  h3 {
    margin: 0 0 12px;
    font-size: 0.9rem;
  }

  .section-header h2 {
    margin: 0;
  }

  .storage-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
  }

  .storage-grid section,
  .section-panel {
    padding: 16px;
  }

  .storage-grid section + section {
    border-left: 1px solid var(--c-border-muted);
  }

  dl {
    display: grid;
    gap: 8px;
    margin: 0;
  }

  dl > div {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 12px;
    min-width: 0;
  }

  dt {
    color: var(--c-text-muted);
    font-size: 0.8rem;
  }

  dd {
    margin: 0;
    text-align: right;
    font-variant-numeric: tabular-nums;
    font-weight: 600;
    font-size: 0.82rem;
  }

  .sample-time,
  .note {
    margin: 0;
    color: var(--c-text-muted);
    font-size: 0.76rem;
  }

  .note {
    margin-top: 10px;
  }

  .volume-values,
  .maintenance-details {
    grid-template-columns: repeat(2, minmax(0, 1fr));
    column-gap: 32px;
  }

  meter {
    display: block;
    width: 100%;
    height: 12px;
    margin-top: 10px;
  }

  .cycle {
    margin-top: 16px;
    padding-top: 14px;
    border-top: 1px solid var(--c-border-muted);
  }

  .cycle-counts {
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: 16px;
    margin-top: 14px;
  }

  .cycle-counts > div {
    flex-direction: column;
    gap: 4px;
  }

  @media (max-width: 1150px) {
    .storage-grid {
      grid-template-columns: 1fr;
    }

    .storage-grid section + section {
      border-left: 0;
      border-top: 1px solid var(--c-border-muted);
    }
  }

  @media (max-width: 700px) {
    .maintenance-details,
    .volume-values {
      grid-template-columns: 1fr;
    }

    .cycle-counts {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
</style>
