import { expect, test } from "@playwright/test";

const baseURL = process.env.BENCHDB_E2E_BASE_URL ?? "http://localhost:8099";

test("keeps keyboard focus inside the open navigation drawer on narrow screens", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${baseURL}/`);
  const menu = page.getByRole("button", { name: "Open navigation" });
  await menu.click();
  const drawer = page.locator("#app-sidebar.drawer-open");
  await expect(drawer).toBeVisible();
  await expect(page.locator(".app-content")).toHaveJSProperty("inert", true);

  const focusInsideDrawer = () =>
    page.evaluate(() => document.getElementById("app-sidebar")?.contains(document.activeElement) ?? false);
  for (let i = 0; i < 15; i++) {
    await page.keyboard.press("Tab");
    expect(await focusInsideDrawer(), `Tab ${i + 1} left the drawer`).toBe(true);
  }
  for (let i = 0; i < 15; i++) {
    await page.keyboard.press("Shift+Tab");
    expect(await focusInsideDrawer(), `Shift+Tab ${i + 1} left the drawer`).toBe(true);
  }

  await page.keyboard.press("Escape");
  await expect(drawer).toHaveCount(0);
  await expect(menu).toBeFocused();
  await expect(page.locator(".app-content")).toHaveJSProperty("inert", false);
});

test("shows a focus outline on the sidebar search in forced-colors mode", async ({ page }) => {
  await page.emulateMedia({ forcedColors: "active" });
  await page.goto(`${baseURL}/`);
  const search = page.getByRole("searchbox", { name: "Series search query" });
  for (let i = 0; i < 12 && !(await search.evaluate((el) => el === document.activeElement)); i++) {
    await page.keyboard.press("Tab");
  }
  await expect(search).toBeFocused();
  expect(await search.evaluate((el) => getComputedStyle(el).outlineStyle)).toBe("solid");
});
