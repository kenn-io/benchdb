/** Machine line colors in fixed order. Red and green are left out because the
 * dashboard uses them for regressed and improved status. Every chart that uses
 * these colors also labels each machine in a legend. */
const MACHINE_COLORS = ["#2563eb", "#d97706", "#7c3aed", "#0891b2", "#db2777", "#4f46e5"] as const;

export function machineColor(index: number): string {
  return MACHINE_COLORS[index % MACHINE_COLORS.length]!;
}
