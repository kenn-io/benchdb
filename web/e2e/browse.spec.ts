import { test, expect } from "@playwright/test";

const baseURL = process.env.BENCHDB_E2E_BASE_URL ?? "http://localhost:8099";

test("browse lists the seeded series, searches, and opens its trend", async ({ page }) => {
  await page.goto(`${baseURL}/series`);

  // The seeded demo series appears as one fleet-level row with history status.
  const row = page.locator("table.browse-table tbody tr", { hasText: "ingest-events-10m" });
  await expect(row).toBeVisible();
  await expect(row.locator(".history-cell")).toContainText(/points/);
  await expect(row.locator(".badge")).toBeVisible();

  // The top-bar search narrows server-side via ?q= and survives reload (URL state).
  await page.getByRole("searchbox", { name: /series search query/i }).fill("ingest-events-10m");
  await page.getByRole("searchbox", { name: /series search query/i }).press("Enter");
  await expect(page).toHaveURL(/\?q=ingest-events-10m/);
  await expect(page.locator("table.browse-table tbody tr")).toHaveCount(1);
  await page.reload();
  await expect(page.locator("table.browse-table tbody tr")).toHaveCount(1);

  // Row click opens the benchmark trend via SPA navigation. The default range
  // is anchored at the newest series point, so the seeded history renders.
  await page.locator("table.browse-table tbody tr a").first().click();
  await expect(page).toHaveURL(/\/series\//);
  await expect(page.locator('.fleet-chart canvas, .chart-wrap canvas').first()).toBeVisible();

  // A search with no matches shows the empty state, not an error.
  await page.goto(`${baseURL}/series?q=definitely-not-a-benchmark`);
  await expect(page.getByText(/no series match/i)).toBeVisible();
});
