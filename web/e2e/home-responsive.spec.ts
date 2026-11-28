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

test("home uses zero-gutter overlay scrollbars on wide and narrow viewports", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 320 });
  await page.goto(`${baseURL}/`);

  const panel = page.locator("section.table-panel");
  await expect(panel.locator("tbody tr").first()).toBeVisible();

  const wideMetrics = await panel.evaluate((el: HTMLElement) => {
    const style = getComputedStyle(el);
    const borders =
      Number.parseFloat(style.borderLeftWidth) + Number.parseFloat(style.borderRightWidth);
    const head = el.querySelector("thead");
    const track = el.querySelector<HTMLElement>(".kit-scrollbar--y");
    return {
      gutter: Math.round(el.offsetWidth - el.clientWidth - borders),
      overflowY: el.scrollHeight - el.clientHeight,
      headHeight: head ? head.getBoundingClientRect().height : 0,
      trackTop: track ? Number.parseFloat(track.style.top) : -1,
    };
  });
  expect(wideMetrics.overflowY).toBeGreaterThan(0);
  expect(wideMetrics.gutter).toBe(0);
  expect(wideMetrics.trackTop).toBeGreaterThanOrEqual(wideMetrics.headHeight - 1);

  const panelBox = (await panel.boundingBox())!;
  await page.mouse.move(panelBox.x + panelBox.width / 2, panelBox.y + panelBox.height / 2);
  await page.mouse.wheel(0, 120);
  await expect.poll(() => panel.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);

  await page.setViewportSize({ width: 390, height: 500 });
  const content = page.locator(".app-content");
  const narrowMetrics = await content.evaluate((el: HTMLElement) => {
    const style = getComputedStyle(el);
    const borders =
      Number.parseFloat(style.borderLeftWidth) + Number.parseFloat(style.borderRightWidth);
    const track = el.parentElement?.querySelector<HTMLElement>(":scope > .kit-scrollbar--y");
    return {
      gutter: Math.round(el.offsetWidth - el.clientWidth - borders),
      overflowY: el.scrollHeight - el.clientHeight,
      hasHostTrack: track !== null && track !== undefined,
    };
  });
  expect(narrowMetrics.overflowY).toBeGreaterThan(0);
  expect(narrowMetrics.gutter).toBe(0);
  expect(narrowMetrics.hasHostTrack).toBe(true);
});
