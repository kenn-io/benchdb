<script lang="ts">
  import { onDestroy } from "svelte";

  import { copyPlainText } from "../clipboard";
  import {
    clipboardMeasurementValue,
    exactMeasurement,
    formatMeasurement,
  } from "../format";

  let {
    value,
    unit,
    missing = "—",
  }: {
    value: number | null;
    unit: string | null;
    missing?: string;
  } = $props();

  let copied = $state(false);
  let resetTimer: ReturnType<typeof setTimeout> | undefined;

  async function copyValue(event: MouseEvent) {
    event.preventDefault();
    event.stopPropagation();
    if (value === null) return;
    try {
      copied = await copyPlainText(clipboardMeasurementValue(value));
    } catch {
      copied = false;
    }
    if (resetTimer !== undefined) clearTimeout(resetTimer);
    resetTimer = setTimeout(() => (copied = false), 1500);
  }

  onDestroy(() => {
    if (resetTimer !== undefined) clearTimeout(resetTimer);
  });
</script>

{#if value === null}
  <span>{missing}</span>
{:else}
  <button
    type="button"
    class="measurement-value numeric-text"
    title={`${exactMeasurement(value, unit)} — click to copy the exact number`}
    aria-label={`${formatMeasurement(value, unit)}; exact value ${exactMeasurement(value, unit)}; click to copy`}
    onclick={copyValue}
  >{formatMeasurement(value, unit)}</button>
  <span class="sr-only" aria-live="polite">{copied ? "Exact value copied" : ""}</span>
{/if}

<style>
  .measurement-value {
    appearance: none;
    padding: 0;
    border: 0;
    background: transparent;
    color: inherit;
    cursor: copy;
    font: inherit;
    text-align: inherit;
    text-decoration: underline dotted color-mix(in srgb, currentColor 45%, transparent);
    text-underline-offset: 3px;
  }
  .measurement-value:hover,
  .measurement-value:focus-visible {
    color: var(--c-accent);
    text-decoration-style: solid;
  }
</style>
