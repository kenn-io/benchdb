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
  <span class="measurement-copy">
    <button
      type="button"
      class="measurement-value numeric-text"
      title={`${exactMeasurement(value, unit)} — click to copy the exact number`}
      aria-label={`${formatMeasurement(value, unit)}; exact value ${exactMeasurement(value, unit)}; click to copy`}
      onclick={copyValue}
    >{formatMeasurement(value, unit)}</button>
    {#if copied}<span class="copy-feedback" role="status">Copied</span>{/if}
  </span>
{/if}

<style>
  .measurement-copy {
    position: relative;
    display: inline-block;
  }
  .measurement-value {
    appearance: none;
    padding: 0;
    border: 0;
    background: transparent;
    color: inherit;
    cursor: pointer;
    font: inherit;
    text-align: inherit;
    text-decoration: underline dotted color-mix(in srgb, currentColor 45%, transparent);
    text-underline-offset: 3px;
  }
  .copy-feedback {
    position: absolute;
    z-index: 2;
    bottom: calc(100% + 0.35rem);
    left: 50%;
    padding: 0.22rem 0.42rem;
    transform: translateX(-50%);
    border: 1px solid var(--c-border);
    border-radius: 0.3rem;
    background: var(--c-surface-raised);
    box-shadow: 0 2px 8px color-mix(in srgb, var(--c-text) 14%, transparent);
    color: var(--c-text);
    font-family: system-ui, sans-serif;
    font-size: 0.72rem;
    font-weight: 650;
    line-height: 1.2;
    pointer-events: none;
    white-space: nowrap;
  }
  .measurement-value:hover,
  .measurement-value:focus-visible {
    color: var(--c-accent);
    text-decoration-style: solid;
  }
</style>
