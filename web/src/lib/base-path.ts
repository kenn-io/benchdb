/** The server sets the base element for this deployment, including deep links. */
export function basePath(): string {
  const base = document.querySelector("base");
  return base ? new URL(base.href).pathname.replace(/\/$/, "") : "";
}

/** Resolve app paths without changing absolute external links. */
export function appURL(url: string): string {
  if (url.startsWith("#")) return location.pathname + location.search + url;
  return url.startsWith("/") && !url.startsWith("//") ? basePath() + url : url;
}
