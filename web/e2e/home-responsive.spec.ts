import { expect, test } from "@playwright/test";

const baseURL = process.env.BENCHDB_E2E_BASE_URL ?? "http://localhost:8099";

test("home uses the stacked recent-runs layout at narrow desktop widths", async ({ page }) => {
  await page.setViewportSize({ width: 1024, height: 768 });
  await page.goto(`${baseURL}/`);

  const table = page.locator("table.runs-table");
  await expect(table).toBeVisible();
  await expect(table).toHaveCSS("display", "block");
  await expect(table.locator("thead")).toHaveCSS("display", "none");
  await expect(table.locator("tbody tr").first().locator('td[data-label="Results"]')).toHaveCSS(
    "text-align",
    "left",
  );
});

test("home scrolls as one page below the stacked-table breakpoint", async ({ page }) => {
  // A short phone viewport, so the stacked run cards are taller than the screen.
  await page.setViewportSize({ width: 390, height: 500 });
  await page.goto(`${baseURL}/`);
  const panel = page.locator("section.table-panel");
  await expect(panel.locator("tbody tr").first()).toBeVisible();

  // Stacked rows are cards; a scroll area inside the scrolling page is hard to
  // use on touch and shrinks to nothing under zoom. The page scrolls instead.
  const panelOverflow = await panel.evaluate((el) => el.scrollHeight - el.clientHeight);
  expect(panelOverflow).toBeLessThanOrEqual(1);
  const pageOverflow = await page.locator(".app-content").evaluate((el) => el.scrollHeight - el.clientHeight);
  expect(pageOverflow).toBeGreaterThan(0);
});
