import { test, expect } from "@playwright/test";

const baseURL = process.env.BENCHDB_E2E_BASE_URL ?? "http://localhost:8099";

test("assets, API requests, native links, and reloads stay under the public URL", async ({ page, context }) => {
  const escaped: string[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (url.origin === new URL(baseURL).origin && !url.href.startsWith(baseURL + "/")) {
      escaped.push(url.href);
    }
  });
  await page.goto(baseURL + "/");
  const benchmarks = page.getByRole("link", { name: "Benchmarks", exact: true });
  await expect(benchmarks).toBeVisible();
  expect(await benchmarks.evaluate((link: HTMLAnchorElement) => link.href)).toBe(baseURL + "/series");
  await benchmarks.click();
  await expect(page).toHaveURL(baseURL + "/series");
  await expect(page.locator("tbody tr").first()).toBeVisible();
  await page.reload();
  await expect(page.locator("tbody tr").first()).toBeVisible();

  const tab = await context.newPage();
  await tab.goto(await page.getByRole("link", { name: "Account", exact: true }).evaluate((link: HTMLAnchorElement) => link.href));
  await expect(tab.getByRole("link", { name: "Sign in", exact: true })).toHaveAttribute("href", new URL(baseURL).pathname.replace(/\/$/, "") + "/api/auth/login");
  await tab.close();
  expect(escaped).toEqual([]);
});
