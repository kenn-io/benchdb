import { test, expect } from "@playwright/test";

const baseURL = process.env.BENCHDB_E2E_BASE_URL ?? "http://localhost:8099";

test("storage shows the running server's usage and hourly maintenance schedule", async ({ page, request }) => {
  await page.goto(`${baseURL}/`);
  await page.getByRole("link", { name: "Storage", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Storage", exact: true })).toBeVisible();
  await expect(page.getByText("Every 1 hour", { exact: true })).toBeVisible();
  await expect(page.getByText("Scheduled", { exact: true })).toBeVisible();
  await expect(page.getByRole("region", { name: "PostgreSQL" })).toContainText(/Database size\s*[\d.]+ [KMGT]B/);
  await expect(page.getByRole("region", { name: "Parquet archive" })).toContainText("Archived rows");
  // The e2e server sets no BENCHDB_ARCHIVE_RETENTION, as an upgraded local deployment may not.
  const retention = page.getByRole("region", { name: "Retention" });
  await expect(retention).toContainText("Cleanup paused");
  await expect(retention).toContainText("BENCHDB_ARCHIVE_RETENTION");
  await expect(page.getByRole("meter", { name: "Working space used" })).toBeVisible();
  await expect(page.getByRole("meter", { name: "Archive volume used" })).toBeVisible();

  const response = await request.get(`${baseURL}/api/storage`);
  expect(response.ok()).toBeTruthy();
  const status = await response.json();
  expect(status.postgres_bytes).toBeGreaterThan(0);
  expect(status.backend).toBe("file");
  expect(status.volume_total_bytes).toBeGreaterThan(0);
  expect(status.reclamation_paused).toBe(true);
  expect(status.maintenance.enabled).toBe(true);
  expect(status.maintenance.last_started_at).toBeUndefined();
  expect(Date.parse(status.maintenance.next_run_at)).toBeGreaterThan(Date.now());

  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(page.getByRole("button", { name: "Refresh", exact: true })).toBeEnabled();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.reload();
  await expect(page.getByText("Scheduled", { exact: true })).toBeVisible();
});
