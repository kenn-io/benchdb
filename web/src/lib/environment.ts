export interface EnvironmentSummary {
  epoch: string | null;
  highlights: string[];
}

/** summarizeEnvironment extracts the few environment facts useful during
 * benchmark triage. The full submitted object remains available separately;
 * multiline build and OS payloads never belong in page headers or tables. */
export function summarizeEnvironment(context: Record<string, unknown>): EnvironmentSummary {
  const epoch = compactEpoch(stringValue(context["environment_epoch_id"]));
  const highlights = [
    osLabel(stringValue(context["os_release"])),
    kernelLabel(stringValue(context["kernel_release"])),
    goLabel(context),
    tuningLabel("governor", stringValue(context["governor"])),
    binaryLabel("SMT", stringValue(context["smt"]), { "0": "off", "1": "on" }),
    binaryLabel("turbo", stringValue(context["turbo"]), { "0": "on", "1": "off" }),
  ].filter((value): value is string => value !== null);

  return { epoch, highlights };
}

function stringValue(value: unknown): string | null {
  return typeof value === "string" && value.trim() !== "" ? value.trim() : null;
}

function compactEpoch(value: string | null): string | null {
  if (value === null) return null;
  return value.length <= 18 ? value : `${value.slice(0, 14)}…${value.slice(-4)}`;
}

function osLabel(value: string | null): string | null {
  if (value === null) return null;
  const pretty = value
    .split("\n")
    .find((line) => line.startsWith("PRETTY_NAME="))
    ?.slice("PRETTY_NAME=".length)
    .replace(/^"|"$/g, "");
  return pretty?.trim() || firstLine(value);
}

function kernelLabel(value: string | null): string | null {
  return value === null ? null : `kernel ${firstLine(value)}`;
}

function goLabel(context: Record<string, unknown>): string | null {
  const version = stringValue(context["go_version"]) ?? stringValue(context["go_environment"])?.split("\n")[0] ?? null;
  return version === null ? null : version.replace(/^go(?=\d)/, "Go ");
}

function tuningLabel(name: string, value: string | null): string | null {
  return value === null ? null : `${name} ${firstLine(value)}`;
}

function binaryLabel(
  name: string,
  value: string | null,
  labels: Record<string, string>,
): string | null {
  if (value === null) return null;
  return `${name} ${labels[firstLine(value)] ?? firstLine(value)}`;
}

function firstLine(value: string): string {
  return value.split("\n", 1)[0]!.trim();
}
