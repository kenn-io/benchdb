<script lang="ts">
  import MonitorIcon from "@lucide/svelte/icons/monitor";
  import MoonIcon from "@lucide/svelte/icons/moon";
  import SunIcon from "@lucide/svelte/icons/sun";

  import { cycleTheme, themeChoice, type ThemeChoice } from "../theme.svelte";

  let { compact = false }: { compact?: boolean } = $props();

  const choiceLabel: Record<ThemeChoice, string> = {
    system: "System",
    light: "Light",
    dark: "Dark",
  };
  const nextChoice: Record<ThemeChoice, ThemeChoice> = {
    system: "light",
    light: "dark",
    dark: "system",
  };
  const icons = { system: MonitorIcon, light: SunIcon, dark: MoonIcon };

  let choice = $derived(themeChoice());
  let label = $derived(
    `Theme: ${choiceLabel[choice]} (switch to ${choiceLabel[nextChoice[choice]]})`,
  );
  const Icon = $derived(icons[choice]);
</script>

<button type="button" class="theme-toggle" class:compact aria-label={label} title={label} onclick={cycleTheme}>
  <Icon size={17} strokeWidth={1.75} aria-hidden="true" />
  {#if !compact}<span>{choiceLabel[choice]} theme</span>{/if}
</button>

<style>
  .theme-toggle {
    width: 100%;
    height: 34px;
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 0 10px;
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--c-text-muted);
    font: inherit;
    font-size: 0.82rem;
    font-weight: 600;
    cursor: pointer;
  }

  .theme-toggle.compact {
    width: 38px;
    justify-content: center;
    padding: 0;
  }

  .theme-toggle:hover {
    background: var(--c-surface-hover);
    color: var(--c-text);
  }

  .theme-toggle:focus-visible {
    outline: 2px solid var(--c-accent);
    outline-offset: 2px;
  }
</style>
