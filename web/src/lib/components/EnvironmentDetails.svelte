<script lang="ts">
  import { summarizeEnvironment } from "../environment";

  let {
    context,
    label = "Environment details",
  }: {
    context: Record<string, unknown>;
    label?: string;
  } = $props();

  const summary = $derived(summarizeEnvironment(context));
</script>

<details class="environment-details">
  <summary>
    <span>{label}</span>
    {#if summary.epoch}<span class="epoch mono" title={String(context["environment_epoch_id"])}>{summary.epoch}</span>{/if}
  </summary>
  {#if summary.highlights.length > 0}
    <ul class="environment-highlights" aria-label="Environment summary">
      {#each summary.highlights as item (item)}<li>{item}</li>{/each}
    </ul>
  {/if}
  <pre>{JSON.stringify(context, null, 2)}</pre>
</details>

<style>
  .environment-details {
    border: 1px solid var(--c-border-muted);
    border-radius: var(--radius-sm);
    background: var(--c-surface-subtle);
  }

  summary {
    min-height: 34px;
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 7px 10px;
    cursor: pointer;
    color: var(--c-text);
    font-weight: 700;
  }

  .epoch {
    color: var(--c-text-muted);
    font-size: 0.74rem;
    font-weight: 500;
  }

  .environment-highlights {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    margin: 0;
    padding: 0 10px 9px;
    list-style: none;
  }

  .environment-highlights li {
    padding: 2px 7px;
    border: 1px solid var(--c-border-muted);
    border-radius: 999px;
    background: var(--c-surface);
    color: var(--c-text-muted);
    font-size: 0.74rem;
  }

  pre {
    max-height: 360px;
    padding: 10px;
    border-top: 1px solid var(--c-border-muted);
    background: var(--c-bg-inset);
    color: var(--c-text-muted);
    font-size: 0.75rem;
  }
</style>
