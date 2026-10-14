<script lang="ts">
  import { formatChange, type BrowseRow } from "../browse/transform";
  import StatusBadge from "./StatusBadge.svelte";

  let { row }: { row: BrowseRow } = $props();

  const change = $derived(row.changePercent);
  // Name the machine only when the benchmark runs on several.
  const machine = $derived(row.machineNames.length > 1 ? row.changeMachine : null);
  const spoken = $derived.by(() => {
    if (change === null) return "";
    const where = row.changeMachine ? ` on ${row.changeMachine}` : "";
    const size = formatChange(Math.abs(change));
    if (size === formatChange(0)) return `, latest at its history mean${where}`;
    return `, latest ${size.slice(1)} ${change > 0 ? "above" : "below"} its history mean${where}`;
  });
</script>

<span class="benchmark-status">
  <StatusBadge status={row.status} />
  {#if change !== null}
    <span class={`change mono ${row.status}`} aria-hidden="true"
      title={`Latest result vs. its history mean${row.changeMachine ? ` on ${row.changeMachine}` : ""}`}
    >{formatChange(change)}{#if machine}<span class="machine">{machine}</span>{/if}</span>
    <span class="sr-only">{spoken}</span>
  {/if}
</span>

<style>
  .benchmark-status {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 4px 8px;
  }
  .change {
    color: var(--c-text-muted);
    font-size: 0.78rem;
    font-variant-numeric: tabular-nums;
  }
  .change.regressed { color: var(--c-error); font-weight: 600; }
  .change.improved { color: var(--c-success); font-weight: 600; }
  .machine {
    margin-left: 6px;
    color: var(--c-text-muted);
    font-family: Inter, ui-sans-serif, system-ui, sans-serif;
    font-weight: 400;
  }
</style>
