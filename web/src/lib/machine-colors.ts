import type { ResolvedTheme } from "./theme.svelte";

/** Machine line colors per theme. Red, pink, and green are left out because
 * the dashboard uses red and green for regressed and improved status. Every color reaches
 * 3:1 against its theme's chart background, and every chart that uses them
 * also labels each machine in a legend. */
const PALETTES: Record<ResolvedTheme, readonly string[]> = {
  light: ["#2563eb", "#d97706", "#7c3aed", "#0891b2", "#4f46e5", "#0f766e"],
  dark: ["#60a5fa", "#f59e0b", "#a78bfa", "#22d3ee", "#818cf8", "#2dd4bf"],
};

// FNV-1a: a small, stable string hash, so a machine's color depends only on
// its name and not on which other machines a chart happens to show.
function nameHash(name: string): number {
  let hash = 0x811c9dc5;
  for (let i = 0; i < name.length; i++) {
    hash ^= name.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193);
  }
  return hash >>> 0;
}

/** machineColors gives each machine its name's palette color, so a machine
 * keeps its color across cards, filters, and pages. When two machines in one
 * chart want the same color, the later name in sort order takes the next free
 * one; past the palette's size, colors repeat. */
export function machineColors(names: readonly string[], theme: ResolvedTheme): Map<string, string> {
  const palette = PALETTES[theme];
  const taken = new Set<number>();
  const colors = new Map<string, string>();
  for (const name of [...new Set(names)].sort()) {
    let index = nameHash(name) % palette.length;
    while (taken.size < palette.length && taken.has(index)) index = (index + 1) % palette.length;
    taken.add(index);
    colors.set(name, palette[index]!);
  }
  return colors;
}
